package version

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/kotsadm/pkg/config"
	"github.com/replicatedhq/kots/kotsadm/pkg/downstream"
	"github.com/replicatedhq/kots/kotsadm/pkg/gitops"
	"github.com/replicatedhq/kots/kotsadm/pkg/kotsutil"
	"github.com/replicatedhq/kots/kotsadm/pkg/persistence"
	"github.com/replicatedhq/kots/kotsadm/pkg/secrets"
	"github.com/replicatedhq/kots/kotsadm/pkg/store"
	"github.com/replicatedhq/kots/kotsadm/pkg/version/types"
	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
	"k8s.io/client-go/kubernetes/scheme"
	applicationv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

// GetNextAppSequence determines next available sequence for this app
// we shouldn't assume that a.CurrentSequence is accurate. Returns 0 if currentSequence is nil
func GetNextAppSequence(appID string, currentSequence *int64) (int64, error) {
	newSequence := 0
	if currentSequence != nil {
		db := persistence.MustGetPGSession()
		row := db.QueryRow(`select max(sequence) from app_version where app_id = $1`, appID)
		if err := row.Scan(&newSequence); err != nil {
			return 0, errors.Wrap(err, "failed to find current max sequence in row")
		}
		newSequence++
	}
	return int64(newSequence), nil
}

// CreateFirstVersion works much likst CreateVersion except that it assumes version 0
// and never attempts to calculate a diff, or look at previous versions
func CreateFirstVersion(appID string, filesInDir string, source string) (int64, error) {
	return createVersion(appID, filesInDir, source, nil)
}

// CreateVersion creates a new version of the app in the database, but the caller
// is responsible for uploading the archive to s3
func CreateVersion(appID string, filesInDir string, source string, currentSequence int64) (int64, error) {
	return createVersion(appID, filesInDir, source, &currentSequence)
}

// this is the common, internal function to create an app version, used in both
// new and updates to apps
func createVersion(appID string, filesInDir string, source string, currentSequence *int64) (int64, error) {
	kotsKinds, err := kotsutil.LoadKotsKindsFromPath(filesInDir)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to read kots kinds")
	}

	supportBundleSpec, err := kotsKinds.Marshal("troubleshoot.replicated.com", "v1beta1", "Collector")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal support bundle spec")
	}
	analyzersSpec, err := kotsKinds.Marshal("troubleshoot.replicated.com", "v1beta1", "Analyzer")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal analyzer spec")
	}
	preflightSpec, err := kotsKinds.Marshal("troubleshoot.replicated.com", "v1beta1", "Preflight")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal preflight spec")
	}

	appSpec, err := kotsKinds.Marshal("app.k8s.io", "v1beta1", "Application")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal app spec")
	}
	kotsAppSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Application")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal kots app spec")
	}
	kotsInstallationSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Installation")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal kots installation spec")
	}
	backupSpec, err := kotsKinds.Marshal("velero.io", "v1", "Backup")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal backup spec")
	}

	licenseSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "License")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal license spec")
	}
	configSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Config")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal config spec")
	}
	configValuesSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "ConfigValues")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal configvalues spec")
	}

	db := persistence.MustGetPGSession()

	tx, err := db.Begin()
	if err != nil {
		return 0, errors.Wrap(err, "failed to begin")
	}
	defer tx.Rollback()

	n, err := GetNextAppSequence(appID, currentSequence)
	if err != nil {
		return 0, errors.Wrap(err, "failed to get new app sequence")
	}
	newSequence := int(n)

	query := `insert into app_version (app_id, sequence, created_at, version_label, release_notes, update_cursor, channel_name, encryption_key,
supportbundle_spec, analyzer_spec, preflight_spec, app_spec, kots_app_spec, kots_installation_spec, kots_license, config_spec, config_values, backup_spec)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
ON CONFLICT(app_id, sequence) DO UPDATE SET
created_at = EXCLUDED.created_at,
version_label = EXCLUDED.version_label,
release_notes = EXCLUDED.release_notes,
update_cursor = EXCLUDED.update_cursor,
channel_name = EXCLUDED.channel_name,
encryption_key = EXCLUDED.encryption_key,
supportbundle_spec = EXCLUDED.supportbundle_spec,
analyzer_spec = EXCLUDED.analyzer_spec,
preflight_spec = EXCLUDED.preflight_spec,
app_spec = EXCLUDED.app_spec,
kots_app_spec = EXCLUDED.kots_app_spec,
kots_installation_spec = EXCLUDED.kots_installation_spec,
kots_license = EXCLUDED.kots_license,
config_spec = EXCLUDED.config_spec,
config_values = EXCLUDED.config_values,
backup_spec = EXCLUDED.backup_spec`
	_, err = tx.Exec(query, appID, newSequence, time.Now(),
		kotsKinds.Installation.Spec.VersionLabel,
		kotsKinds.Installation.Spec.ReleaseNotes,
		kotsKinds.Installation.Spec.UpdateCursor,
		kotsKinds.Installation.Spec.ChannelName,
		kotsKinds.Installation.Spec.EncryptionKey,
		supportBundleSpec,
		analyzersSpec,
		preflightSpec,
		appSpec,
		kotsAppSpec,
		kotsInstallationSpec,
		licenseSpec,
		configSpec,
		configValuesSpec,
		backupSpec)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to insert app version")
	}

	appName := kotsKinds.KotsApplication.Spec.Title
	if appName == "" {
		a, err := store.GetStore().GetApp(appID)
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to get app")
		}

		appName = a.Name
	}

	appIcon := kotsKinds.KotsApplication.Spec.Icon

	query = "update app set current_sequence = $1, name = $2, icon_uri = $3 where id = $4"
	_, err = tx.Exec(query, int64(newSequence), appName, appIcon, appID)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to update app")
	}

	previousArchiveDir := ""
	if currentSequence != nil {
		// Get the previous archive, we need this to calculate the diff
		previousDir, err := store.GetStore().GetAppVersionArchive(appID, *currentSequence)
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to get previous archive")
		}

		previousArchiveDir = previousDir
	}

	downstreams, err := store.GetStore().ListDownstreamsForApp(appID)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to list downstreams")
	}

	if err := secrets.ReplaceSecretsInPath(filesInDir); err != nil {
		return int64(0), errors.Wrap(err, "failed to replace secrets")
	}

	for _, d := range downstreams {
		downstreamStatus := "pending"
		if currentSequence == nil && kotsKinds.Config != nil {
			downstreamStatus = "pending_config"
		} else if kotsKinds.Preflight != nil {
			downstreamStatus = "pending_preflight"
		}

		if currentSequence != nil {
			// check if version needs additional configuration
			t, err := config.NeedsConfiguration(configSpec, configValuesSpec, licenseSpec)
			if err != nil {
				return int64(0), errors.Wrap(err, "failed to check if version needs configuration")
			}
			if t {
				downstreamStatus = "pending_config"
			}
		}

		diffSummary, diffSummaryError := "", ""
		if currentSequence != nil {
			// diff this release from the last release
			diff, err := downstream.DiffAppVersionsForDownstream(d.Name, filesInDir, previousArchiveDir, kotsKinds.KustomizeVersion())
			if err != nil {
				diffSummaryError = errors.Wrap(err, "failed to diff").Error()
			} else {
				b, err := json.Marshal(diff)
				if err != nil {
					diffSummaryError = errors.Wrap(err, "failed to marshal diff").Error()
				}
				diffSummary = string(b)
			}
		}

		commitURL := ""
		downstreamGitOps, err := gitops.GetDownstreamGitOps(appID, d.ClusterID)
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to get downstream gitops")
		}
		if downstreamGitOps != nil {
			a, err := store.GetStore().GetApp(appID)
			if err != nil {
				return int64(0), errors.Wrap(err, "failed to get app")
			}
			createdCommitURL, err := gitops.CreateGitOpsCommit(downstreamGitOps, a.Slug, a.Name, int(newSequence), filesInDir, d.Name)
			if err != nil {
				return int64(0), errors.Wrap(err, "failed to create gitops commit")
			}
			commitURL = createdCommitURL
		}

		query = `insert into app_downstream_version (app_id, cluster_id, sequence, parent_sequence, created_at, version_label, status, source, diff_summary, diff_summary_error, git_commit_url, git_deployable) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
		_, err = tx.Exec(query, appID, d.ClusterID, newSequence, newSequence, time.Now(),
			kotsKinds.Installation.Spec.VersionLabel, downstreamStatus, source,
			diffSummary, diffSummaryError, commitURL, commitURL != "")
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to create downstream version")
		}
	}

	if err := store.GetStore().CreateAppVersionArchive(appID, int64(newSequence), filesInDir); err != nil {
		return int64(0), errors.Wrap(err, "failed to create app version archive")
	}

	if err := tx.Commit(); err != nil {
		return int64(0), errors.Wrap(err, "failed to commit")
	}

	return int64(newSequence), nil
}

// return the list of versions available for an app
func GetVersions(appID string) ([]types.AppVersion, error) {
	db := persistence.MustGetPGSession()
	query := `select sequence from app_version where app_id = $1 order by update_cursor asc, sequence asc`
	rows, err := db.Query(query, appID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query app_version table")
	}
	defer rows.Close()

	versions := []types.AppVersion{}
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			return nil, errors.Wrap(err, "failed to scan sequence from app_version table")
		}

		v, err := Get(appID, sequence)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get version")
		}
		if v != nil {
			versions = append(versions, *v)
		}
	}

	return versions, nil
}

func Get(appID string, sequence int64) (*types.AppVersion, error) {
	db := persistence.MustGetPGSession()
	query := `select sequence, update_cursor, version_label, created_at, release_notes, status, applied_at from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, appID, sequence)

	var updateCursor sql.NullInt32
	var versionLabel sql.NullString
	var createdOn sql.NullTime
	var releaseNotes sql.NullString
	var status sql.NullString
	var deployedAt sql.NullTime

	v := types.AppVersion{}
	if err := row.Scan(&v.Sequence, &updateCursor, &versionLabel, &createdOn, &releaseNotes, &status, &deployedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, errors.Wrap(err, "failed to scan")
	}

	if updateCursor.Valid {
		v.UpdateCursor = int(updateCursor.Int32)
	} else {
		v.UpdateCursor = -1
	}
	if createdOn.Valid {
		v.CreatedOn = &createdOn.Time
	}
	if deployedAt.Valid {
		v.DeployedAt = &deployedAt.Time
	}

	v.VersionLabel = versionLabel.String
	v.ReleaseNotes = releaseNotes.String
	v.Status = status.String

	return &v, nil
}

// DeployVersion deploys the version for the given sequence
func DeployVersion(appID string, sequence int64) error {
	db := persistence.MustGetPGSession()

	tx, err := db.Begin()
	if err != nil {
		return errors.Wrap(err, "failed to begin")
	}
	defer tx.Rollback()

	query := `update app_downstream set current_sequence = $1 where app_id = $2`
	_, err = tx.Exec(query, sequence, appID)
	if err != nil {
		return errors.Wrap(err, "failed to update app downstream current sequence")
	}

	query = `update app_downstream_version set status = 'deployed', applied_at = $3 where sequence = $1 and app_id = $2`
	_, err = tx.Exec(query, sequence, appID, time.Now())
	if err != nil {
		return errors.Wrap(err, "failed to update app downstream version status")
	}

	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "failed to commit")
	}

	return nil
}

func GetRealizedLinksFromAppSpec(appID string, sequence int64) ([]types.RealizedLink, error) {
	db := persistence.MustGetPGSession()
	query := `select app_spec, kots_app_spec from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, appID, sequence)

	var appSpecStr sql.NullString
	var kotsAppSpecStr sql.NullString
	if err := row.Scan(&appSpecStr, &kotsAppSpecStr); err != nil {
		if err == sql.ErrNoRows {
			return []types.RealizedLink{}, nil
		}
		return nil, errors.Wrap(err, "failed to scan")
	}

	if appSpecStr.String == "" {
		return []types.RealizedLink{}, nil
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode([]byte(appSpecStr.String), nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode app spec yaml")
	}
	appSpec := obj.(*applicationv1beta1.Application)

	obj, _, err = decode([]byte(kotsAppSpecStr.String), nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode kots app spec yaml")
	}
	kotsAppSpec := obj.(*kotsv1beta1.Application)

	realizedLinks := []types.RealizedLink{}
	for _, link := range appSpec.Spec.Descriptor.Links {
		rewrittenURL := link.URL
		for _, port := range kotsAppSpec.Spec.ApplicationPorts {
			if port.ApplicationURL == link.URL {
				rewrittenURL = fmt.Sprintf("http://localhost:%d", port.LocalPort)
			}
		}
		realizedLink := types.RealizedLink{
			Title: link.Description,
			Uri:   rewrittenURL,
		}
		realizedLinks = append(realizedLinks, realizedLink)
	}

	return realizedLinks, nil
}

func GetForwardedPortsFromAppSpec(appID string, sequence int64) ([]types.ForwardedPort, error) {
	db := persistence.MustGetPGSession()
	query := `select app_spec, kots_app_spec from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, appID, sequence)

	var appSpecStr sql.NullString
	var kotsAppSpecStr sql.NullString
	if err := row.Scan(&appSpecStr, &kotsAppSpecStr); err != nil {
		if err == sql.ErrNoRows {
			return []types.ForwardedPort{}, nil
		}
		return nil, errors.Wrap(err, "failed to scan")
	}

	if appSpecStr.String == "" || kotsAppSpecStr.String == "" {
		return []types.ForwardedPort{}, nil
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode([]byte(appSpecStr.String), nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode app spec yaml")
	}
	appSpec := obj.(*applicationv1beta1.Application)

	obj, _, err = decode([]byte(kotsAppSpecStr.String), nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode kots app spec yaml")
	}
	kotsAppSpec := obj.(*kotsv1beta1.Application)

	if len(kotsAppSpec.Spec.ApplicationPorts) == 0 {
		return []types.ForwardedPort{}, nil
	}

	ports := []types.ForwardedPort{}
	for _, link := range appSpec.Spec.Descriptor.Links {
		for _, port := range kotsAppSpec.Spec.ApplicationPorts {
			if port.ApplicationURL == link.URL {
				ports = append(ports, types.ForwardedPort{
					ServiceName:    port.ServiceName,
					ServicePort:    port.ServicePort,
					LocalPort:      port.LocalPort,
					ApplicationURL: port.ApplicationURL,
				})
			}

		}
	}

	return ports, nil
}
