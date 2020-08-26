package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/kotsadm/pkg/kotsutil"
	"github.com/replicatedhq/kots/kotsadm/pkg/license"
	"github.com/replicatedhq/kots/kotsadm/pkg/logger"
	"github.com/replicatedhq/kots/kotsadm/pkg/online"
	installationtypes "github.com/replicatedhq/kots/kotsadm/pkg/online/types"
	"github.com/replicatedhq/kots/kotsadm/pkg/registry"
	"github.com/replicatedhq/kots/kotsadm/pkg/session"
	"github.com/replicatedhq/kots/kotsadm/pkg/store"
	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
	kotslicense "github.com/replicatedhq/kots/pkg/license"
	kotspull "github.com/replicatedhq/kots/pkg/pull"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
)

type SyncLicenseRequest struct {
	LicenseData string `json:"licenseData"`
}

type SyncLicenseResponse struct {
	ID              string                `json:"id"`
	ExpiresAt       time.Time             `json:"expiresAt"`
	ChannelName     string                `json:"channelName"`
	LicenseSequence int64                 `json:"licenseSequence"`
	LicenseType     string                `json:"licenseType"`
	Entitlements    []EntitlementResponse `json:"entitlements"`
}

type GetLicenseResponse struct {
	ID              string                `json:"id"`
	ExpiresAt       time.Time             `json:"expiresAt"`
	ChannelName     string                `json:"channelName"`
	LicenseSequence int64                 `json:"licenseSequence"`
	LicenseType     string                `json:"licenseType"`
	Entitlements    []EntitlementResponse `json:"entitlements"`
}

type EntitlementResponse struct {
	Title string      `json:"title"`
	Value interface{} `json:"value"`
	Label string      `json:"label"`
}

type UploadLicenseRequest struct {
	LicenseData string `json:"licenseData"`
}

type UploadLicenseResponse struct {
	Success        bool   `json:"success"`
	Error          string `json:"error,omitempty"`
	HasPreflight   bool   `json:"hasPreflight"`
	Slug           string `json:"slug"`
	IsAirgap       bool   `json:"isAirgap"`
	NeedsRegistry  bool   `json:"needsRegistry"`
	IsConfigurable bool   `json:"isConfigurable"`
}

type ResumeInstallOnlineRequest struct {
	Slug string `json:"slug"`
}

type ResumeInstallOnlineResponse struct {
	Success        bool   `json:"success"`
	Error          string `json:"error,omitempty"`
	HasPreflight   bool   `json:"hasPreflight"`
	Slug           string `json:"slug"`
	IsConfigurable bool   `json:"isConfigurable"`
}

type GetOnlineInstallStatusErrorResponse struct {
	Error string `json:"error"`
}

func SyncLicense(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, origin, accept, authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	syncLicenseRequest := SyncLicenseRequest{}
	if err := json.NewDecoder(r.Body).Decode(&syncLicenseRequest); err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	sess, err := session.Parse(r.Header.Get("Authorization"))
	if err != nil {
		logger.Error(err)
		w.WriteHeader(401)
		return
	}

	// we don't currently have roles, all valid tokens are valid sessions
	if sess == nil || sess.ID == "" {
		w.WriteHeader(401)
		return
	}

	foundApp, err := store.GetStore().GetAppFromSlug(mux.Vars(r)["appSlug"])
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	latestLicense, err := license.Sync(foundApp, syncLicenseRequest.LicenseData, true)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	entitlements, expiresAt, err := getLicenseEntitlements(latestLicense)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	syncLicenseResponse := SyncLicenseResponse{
		ID:              latestLicense.Spec.LicenseID,
		ChannelName:     latestLicense.Spec.ChannelName,
		LicenseSequence: latestLicense.Spec.LicenseSequence,
		LicenseType:     latestLicense.Spec.LicenseType,
		Entitlements:    entitlements,
		ExpiresAt:       expiresAt,
	}

	JSON(w, 200, syncLicenseResponse)
}

func GetLicense(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, origin, accept, authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	sess, err := session.Parse(r.Header.Get("Authorization"))
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	// we don't currently have roles, all valid tokens are valid sessions
	if sess == nil || sess.ID == "" {
		w.WriteHeader(401)
		return
	}

	appSlug := mux.Vars(r)["appSlug"]
	foundApp, err := store.GetStore().GetAppFromSlug(appSlug)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	license, err := store.GetStore().GetLicenseForApp(foundApp.ID)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	entitlements, expiresAt, err := getLicenseEntitlements(license)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	getLicenseResponse := GetLicenseResponse{
		ID:              license.Spec.LicenseID,
		ChannelName:     license.Spec.ChannelName,
		LicenseSequence: license.Spec.LicenseSequence,
		LicenseType:     license.Spec.LicenseType,
		Entitlements:    entitlements,
		ExpiresAt:       expiresAt,
	}

	JSON(w, 200, getLicenseResponse)
}

func getLicenseEntitlements(license *kotsv1beta1.License) ([]EntitlementResponse, time.Time, error) {
	var expiresAt time.Time
	entitlements := []EntitlementResponse{}

	for key, entititlement := range license.Spec.Entitlements {
		if key == "expires_at" {
			if entititlement.Value.StrVal == "" {
				continue
			}

			expiration, err := time.Parse(time.RFC3339, entititlement.Value.StrVal)
			if err != nil {
				return nil, time.Time{}, errors.Wrap(err, "failed to parse expiration date")
			}
			expiresAt = expiration
		} else if key == "gitops_enabled" {
			/* do nothing */
		} else if !entititlement.IsHidden {
			entitlements = append(entitlements,
				EntitlementResponse{
					Title: entititlement.Title,
					Label: key,
					Value: entititlement.Value.Value(),
				})
		}
	}

	return entitlements, expiresAt, nil
}

func UploadNewLicense(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, origin, accept, authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	uploadLicenseRequest := UploadLicenseRequest{}
	if err := json.NewDecoder(r.Body).Decode(&uploadLicenseRequest); err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	sess, err := session.Parse(r.Header.Get("Authorization"))
	if err != nil {
		logger.Error(err)
		w.WriteHeader(401)
		return
	}

	// we don't currently have roles, all valid tokens are valid sessions
	if sess == nil || sess.ID == "" {
		w.WriteHeader(401)
		return
	}

	// validate the license
	unverifiedLicense, err := kotsutil.LoadLicenseFromBytes([]byte(uploadLicenseRequest.LicenseData))
	if err != nil {
		logger.Error(err)
		w.WriteHeader(400)
		return
	}

	uploadLicenseResponse := UploadLicenseResponse{
		Success: false,
	}

	verifiedLicense, err := kotspull.VerifySignature(unverifiedLicense)
	if err != nil {
		uploadLicenseResponse.Error = "License signature is not valid"
		JSON(w, 400, uploadLicenseResponse)
		return
	}

	disableOutboundConnections := false
	// ignore the error, default to false
	disableOutboundConnections, _ = strconv.ParseBool(os.Getenv("DISABLE_OUTBOUND_CONNECTIONS"))
	if !disableOutboundConnections {
		// sync license
		logger.Info("syncing license with server to retrieve latest version")
		latestLicense, err := kotslicense.GetLatestLicense(verifiedLicense)
		if err != nil {
			logger.Error(err)
			uploadLicenseResponse.Error = err.Error()
			JSON(w, 500, uploadLicenseResponse)
			return
		}
		verifiedLicense = latestLicense
	}

	// check license expiration
	expired, err := kotspull.LicenseIsExpired(verifiedLicense)
	if err != nil {
		logger.Error(err)
		uploadLicenseResponse.Error = err.Error()
		JSON(w, 500, uploadLicenseResponse)
		return
	}
	if expired {
		uploadLicenseResponse.Error = "License is expired"
		JSON(w, 400, uploadLicenseResponse)
		return
	}

	desiredAppName := strings.Replace(verifiedLicense.Spec.AppSlug, "-", " ", 0)
	upstreamURI := fmt.Sprintf("replicated://%s", verifiedLicense.Spec.AppSlug)

	a, err := store.GetStore().CreateApp(desiredAppName, upstreamURI, uploadLicenseRequest.LicenseData, verifiedLicense.Spec.IsAirgapSupported)
	if err != nil {
		logger.Error(err)
		uploadLicenseResponse.Error = err.Error()
		JSON(w, 500, uploadLicenseResponse)
		return
	}

	if !verifiedLicense.Spec.IsAirgapSupported {
		// complete the install online
		pendingApp := installationtypes.PendingApp{
			ID:          a.ID,
			Slug:        a.Slug,
			Name:        a.Name,
			LicenseData: uploadLicenseRequest.LicenseData,
		}
		kotsKinds, err := online.CreateAppFromOnline(&pendingApp, upstreamURI, false)
		if err != nil {
			logger.Error(err)
			uploadLicenseResponse.Error = err.Error()
			JSON(w, 500, uploadLicenseResponse)
			return
		}

		uploadLicenseResponse.IsAirgap = false
		uploadLicenseResponse.HasPreflight = kotsKinds.Preflight != nil
		uploadLicenseResponse.Success = true
		uploadLicenseResponse.Slug = a.Slug
		uploadLicenseResponse.NeedsRegistry = false
		uploadLicenseResponse.IsConfigurable = kotsKinds.Config != nil

		JSON(w, 200, uploadLicenseResponse)
		return
	}

	uploadLicenseResponse.Success = true
	uploadLicenseResponse.IsAirgap = true
	uploadLicenseResponse.Slug = a.Slug

	// This is the comment from the typescript implementation \
	// and i thought it should remain

	// Carefully now, peek at registry credentials to see if we need to prompt for them
	hasKurlRegistry, err := registry.HasKurlRegistry()
	if err != nil {
		logger.Error(err)
		uploadLicenseResponse.Error = err.Error()
		JSON(w, 300, uploadLicenseRequest)
		return
	}
	uploadLicenseResponse.NeedsRegistry = !hasKurlRegistry

	JSON(w, 200, uploadLicenseResponse)
}

func ResumeInstallOnline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, origin, accept, authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	resumeInstallOnlineResponse := ResumeInstallOnlineResponse{
		Success: false,
	}

	resumeInstallOnlineRequest := ResumeInstallOnlineRequest{}
	if err := json.NewDecoder(r.Body).Decode(&resumeInstallOnlineRequest); err != nil {
		logger.Error(err)
		resumeInstallOnlineResponse.Error = err.Error()
		JSON(w, 500, resumeInstallOnlineResponse)
		return
	}

	sess, err := session.Parse(r.Header.Get("Authorization"))
	if err != nil {
		logger.Error(err)
		resumeInstallOnlineResponse.Error = err.Error()
		JSON(w, 401, resumeInstallOnlineResponse)
		return
	}

	// we don't currently have roles, all valid tokens are valid sessions
	if sess == nil || sess.ID == "" {
		resumeInstallOnlineResponse.Error = "Unauthorized"
		JSON(w, 401, resumeInstallOnlineResponse)
		return
	}

	a, err := store.GetStore().GetAppFromSlug(resumeInstallOnlineRequest.Slug)
	if err != nil {
		logger.Error(err)
		resumeInstallOnlineResponse.Error = err.Error()
		JSON(w, 500, resumeInstallOnlineResponse)
		return
	}

	pendingApp := installationtypes.PendingApp{
		ID:   a.ID,
		Slug: a.Slug,
		Name: a.Name,
	}

	// the license data is left in the table
	kotsLicense, err := store.GetStore().GetLicenseForApp(a.ID)
	if err != nil {
		logger.Error(err)
		resumeInstallOnlineResponse.Error = err.Error()
		JSON(w, 500, resumeInstallOnlineResponse)
		return
	}

	// marshal it
	s := serializer.NewYAMLSerializer(serializer.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
	var b bytes.Buffer
	if err := s.Encode(kotsLicense, &b); err != nil {
		logger.Error(err)
		resumeInstallOnlineResponse.Error = err.Error()
		JSON(w, 500, resumeInstallOnlineResponse)
		return
	}

	pendingApp.LicenseData = string(b.Bytes())

	kotsKinds, err := online.CreateAppFromOnline(&pendingApp, fmt.Sprintf("replicated://%s", kotsLicense.Spec.AppSlug), false)
	if err != nil {
		logger.Error(err)
		resumeInstallOnlineResponse.Error = err.Error()
		JSON(w, 500, resumeInstallOnlineResponse)
		return
	}

	resumeInstallOnlineResponse.HasPreflight = kotsKinds.Preflight != nil
	resumeInstallOnlineResponse.Success = true
	resumeInstallOnlineResponse.Slug = a.Slug
	resumeInstallOnlineResponse.IsConfigurable = kotsKinds.Config != nil

	JSON(w, 200, resumeInstallOnlineResponse)
}

func GetOnlineInstallStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, origin, accept, authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	if err := requireValidSession(w, r); err != nil {
		logger.Error(err)
		JSON(w, 401, GetOnlineInstallStatusErrorResponse{
			Error: fmt.Sprintf("failed to validate session: %v", err),
		})
		return
	}

	status, err := store.GetStore().GetPendingInstallationStatus()
	if err != nil {
		logger.Error(err)
		JSON(w, 500, GetOnlineInstallStatusErrorResponse{
			Error: fmt.Sprintf("failed to get install status: %v", err),
		})
		return
	}

	JSON(w, 200, status)
}

// This the handler for license API and should be called by the application only.
func GetPlatformLicenseCompatibility(w http.ResponseWriter, r *http.Request) {
	apps, err := store.GetStore().ListInstalledApps()
	if err != nil {
		if store.GetStore().IsNotFound(err) {
			JSON(w, http.StatusNotFound, struct{}{})
			return
		}
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if len(apps) == 0 {
		JSON(w, http.StatusNotFound, struct{}{})
		return
	}

	if len(apps) > 1 {
		JSON(w, http.StatusBadRequest, struct{}{})
		return
	}

	app := apps[0]
	license, err := store.GetStore().GetLicenseForApp(app.ID)
	if err != nil {
		logger.Error(err)
		JSON(w, http.StatusInternalServerError, struct{}{})
		return
	}

	type licenseFieldType struct {
		Field            string      `json:"field"`
		Title            string      `json:"title"`
		Type             string      `json:"type"`
		Value            interface{} `json:"value"`
		HideFromCustomer bool        `json:"hide_from_customer,omitempty"`
	}

	type licenseType struct {
		LicenseID      string             `json:"license_id"`
		InstallationID string             `json:"installation_id"`
		Assignee       string             `json:"assignee"`
		ReleaseChannel string             `json:"release_channel"`
		LicenseType    string             `json:"license_type"`
		ExpirationTime string             `json:"expiration_time,omitempty"`
		Fields         []licenseFieldType `json:"fields"`
	}

	platformLicense := licenseType{
		LicenseID:      license.Spec.LicenseID,
		InstallationID: app.ID,
		Assignee:       license.Spec.CustomerName,
		ReleaseChannel: license.Spec.ChannelName,
		LicenseType:    license.Spec.LicenseType,
		Fields:         make([]licenseFieldType, 0),
	}

	for k, e := range license.Spec.Entitlements {
		if k == "expires_at" {
			if e.Value.StrVal != "" {
				platformLicense.ExpirationTime = e.Value.StrVal
			}
			continue
		}

		field := licenseFieldType{
			Field:            k,
			Title:            e.Title,
			Type:             e.ValueType,
			Value:            e.Value.Value(),
			HideFromCustomer: e.IsHidden,
		}

		platformLicense.Fields = append(platformLicense.Fields, field)
	}

	JSON(w, http.StatusOK, platformLicense)
	return
}
