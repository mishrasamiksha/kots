import { Request, Response } from "express";
import { Controller, Get, Post, Res, Req } from "@tsed/common";
import jsYaml from "js-yaml";

import { Params } from "../server/params";

@Controller("/api/v1/preflight")
export class PreflightAPI {
  @Get("/:appSlug/:clusterSlug/:sequence")
  async getPreflightStatus(
    @Req() request: Request,
    @Res() response: Response,

  ): Promise<void> {
    const { appSlug, clusterSlug, sequence } = request.params;

    if (!appSlug || !clusterSlug || !sequence) {
      response.send(400);
    }

    const seqInt = parseInt(sequence, 10);

    try {
      // Fetch YAML from the database and return to client with injected key
      const appId = await request.app.locals.stores.kotsAppStore.getIdFromSlug(appSlug);

      const preflightSpecYaml = await request.app.locals.stores.preflightStore.getKotsPreflightSpec(appId, seqInt);

      if (!preflightSpecYaml) {
        console.log(`Preflight spec for slug: ${appSlug} not found`);
        response.send(404);
        return;
      }

      const specJson = jsYaml.load(preflightSpecYaml);
      const params = await Params.getParams();
      const putUrl = `${params.apiAdvertiseEndpoint}/api/v1/preflight/${appSlug}/${clusterSlug}/${sequence}`;
      specJson.spec.uploadResultsTo = putUrl;

      response.send(200, specJson);

    } catch (err) {
      throw err;
    }
  }

  @Post("/:appSlug/:clusterSlug/:sequence")
  async putPreflightStatus(
    @Req() request: Request,
    @Res() response: Response,
  ): Promise<void> {

    const { appSlug, clusterSlug, sequence } = request.params;
    const seqInt = parseInt(sequence, 10);

    if (!appSlug || !clusterSlug || !sequence) {
      response.send(400);
    }
    const preflightResult = request.body;
    const appId = await request.app.locals.stores.kotsAppStore.getIdFromSlug(appSlug);
    const clusterId = await request.app.locals.stores.clusterStore.getIdFromSlug(clusterSlug);

    if (!appId || !clusterId) {
      response.send(400);
    }

    await request.app.locals.stores.kotsAppStore.addKotsPreflight(appId, clusterId, seqInt, preflightResult);
    response.send(200);
  }
}
