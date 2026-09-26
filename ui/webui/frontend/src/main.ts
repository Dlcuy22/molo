import { mount } from "svelte";
import "./app.css";
import App from "./App.svelte";
import EffectWindow from "./lib/components/EffectWindow.svelte";
import { isEffectWindow } from "./lib/store";

// One bundle serves both windows. The route decides which shell mounts, so the
// effect window is not a second build to keep in step.
mount(isEffectWindow ? EffectWindow : App, {
  target: document.getElementById("app")!,
});
