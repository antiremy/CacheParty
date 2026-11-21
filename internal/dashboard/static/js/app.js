import { initApps, selectService } from "./apps.js";
import { initLayout } from "./layout.js";
import { initPurge } from "./purge.js";
import { initSettings } from "./settings.js";
import { initStats, poll } from "./stats.js";
import { state } from "./state.js";

initPurge();
initApps();
initStats();
initLayout();
initSettings();
selectService(state.service, false);
poll();
