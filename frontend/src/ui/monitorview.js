/**
 * The PGM MONITOR WINDOW's view: the panel (picture, meters, return controls),
 * a header with the Refresh button, and a small alert list. Nothing else.
 *
 * Owner: WP-P.
 *
 * This is the page the monitor PROCESS shows — a second Wails window bound to
 * MonitorApp (monitor_app.go) rather than App. app.js mounts it when
 * backend.isMonitorWindow() and then runs exactly the logic it runs for the
 * panel in the application's window: the KVS monitor, the return handlers, the
 * picture handlers and the native overlay controller. So this view exposes the
 * SAME surface createHomeView does — every setter app.js might call — and
 * answers the ones it has no UI for with a no-op. That is what lets one app.js
 * drive two windows.
 *
 * ============================ WHAT IS NOT HERE ==============================
 *
 * No START/STOP, no lamps but MONITOR, no capture controls, no cough mute, no
 * presets, no Settings, no mixer. Every one of those is the application's
 * business and stays in the application's window. A commentator can close
 * this window by mistake and lose nothing but the picture, which the
 * application's "Restart monitor" button — or the next launch — brings back.
 */

import { createPgmPanel, REFRESH_TITLE } from './pgmpanel.js';
import { createLampRow } from './lamps.js';
import { LAMP_NAMES } from './home.js';
import { createErrorLog, describeEntry, formatErrorTime } from './errorlog.js';
import { SEVERITY, normaliseSeverity } from './alerts.js';
import { readPref, writePref, PREF_MONITOR_RAIL_COLLAPSED } from './prefs.js';

export const MONITOR_WINDOW_TITLE = 'PGM Monitor';

export function createMonitorView(handlers) {
  const el = document.createElement('section');
  el.className = 'view view-home view-monitor';

  // --- alerts, compact --------------------------------------------------------
  const errorLog = createErrorLog();
  const alertsRegion = document.createElement('div');
  alertsRegion.className = 'rail-alerts monitor-alerts';
  const alertsList = document.createElement('ul');
  alertsList.className = 'alert-list';
  alertsList.setAttribute('aria-live', 'polite');
  alertsList.setAttribute('aria-label', 'Alerts');
  const alertsClear = document.createElement('button');
  alertsClear.type = 'button';
  alertsClear.className = 'alert-clear';
  alertsClear.textContent = 'Clear all';
  alertsClear.hidden = true;
  alertsClear.addEventListener('click', () => {
    errorLog.clear();
    renderAlerts();
  });
  alertsRegion.append(alertsClear, alertsList);

  // The count on the collapsed strip: an alert that arrives while the column
  // is folded away must still be visible as a number, as in the application.
  const railStripCount = document.createElement('span');
  railStripCount.className = 'rail-strip-attention';
  railStripCount.hidden = true;

  function renderAlerts() {
    const entries = errorLog.entries;
    alertsList.textContent = '';
    alertsRegion.hidden = entries.length === 0;
    alertsClear.hidden = entries.length === 0;
    railStripCount.hidden = entries.length === 0;
    railStripCount.textContent = String(entries.length);
    for (const entry of entries) {
      const li = document.createElement('li');
      li.className = `alert-row alert-row--${entry.severity}`;
      const when = document.createElement('span');
      when.className = 'alert-when';
      when.textContent = formatErrorTime(entry.lastAt);
      const text = document.createElement('span');
      text.className = 'alert-text';
      text.textContent = entry.count > 1 ? `${entry.message} (×${entry.count})` : entry.message;
      li.title = describeEntry(entry);
      const dismiss = document.createElement('button');
      dismiss.type = 'button';
      dismiss.className = 'alert-dismiss';
      dismiss.setAttribute('aria-label', `Dismiss: ${entry.message}`);
      dismiss.textContent = '✕';
      dismiss.addEventListener('click', () => {
        errorLog.dismiss(entry);
        renderAlerts();
      });
      li.append(when, text, dismiss);
      alertsList.appendChild(li);
    }
  }

  function showError(message, severity) {
    errorLog.record(message, normaliseSeverity(severity));
    renderAlerts();
  }
  function showNote(message) {
    showError(message, SEVERITY.NOTE);
  }
  function clearError() {
    errorLog.clear();
    renderAlerts();
  }
  function clearErrorIf(message) {
    if (errorLog.dismissMatching(message) > 0) renderAlerts();
  }

  // --- the panel --------------------------------------------------------------
  const panel = createPgmPanel(handlers, { showError, clearErrorIf, refreshButton: false });

  // --- the header: what this window is, and the one big button -----------------
  const header = document.createElement('header');
  header.className = 'topbar';
  const titleWrap = document.createElement('div');
  titleWrap.className = 'title-wrap';
  const title = document.createElement('h1');
  title.textContent = MONITOR_WINDOW_TITLE;
  const devBadge = document.createElement('span');
  devBadge.className = 'dev-badge';
  devBadge.textContent = 'DEV — fake backend';
  devBadge.hidden = true;
  titleWrap.append(title, devBadge);

  // The lamps app.js updates. Only MONITOR is shown — it is the one about this
  // window — but every name exists so app.js's updates land somewhere.
  const { lamps } = createLampRow(LAMP_NAMES);
  const monitorLampWrap = document.createElement('div');
  monitorLampWrap.className = 'lamps monitor-lamp';
  monitorLampWrap.appendChild(lamps.MONITOR.el);

  const refreshBtn = document.createElement('button');
  refreshBtn.type = 'button';
  refreshBtn.className = 'btn btn-primary monitor-refresh';
  refreshBtn.textContent = 'REFRESH PICTURE';
  refreshBtn.title = REFRESH_TITLE;
  refreshBtn.addEventListener('click', () => handlers.onPictureRefresh());

  const headerBtns = document.createElement('div');
  headerBtns.className = 'topbar-actions';
  headerBtns.append(monitorLampWrap, refreshBtn);
  header.append(titleWrap, headerBtns);

  // --- the body: the stage, then a rail of controls -----------------------------
  const pgmStage = document.createElement('div');
  pgmStage.className = 'pgm-stage';
  pgmStage.append(panel.tileEl, panel.metersEl);
  panel.attachStage(pgmStage);

  const homeMain = document.createElement('div');
  homeMain.className = 'home-main';
  homeMain.append(pgmStage);

  function makeRailSection(titleText, ...children) {
    const section = document.createElement('section');
    section.className = 'rail-section';
    const h = document.createElement('h2');
    h.className = 'rail-section-title';
    h.textContent = titleText;
    section.append(h, ...children);
    return section;
  }

  const controls = document.createElement('div');
  controls.className = 'controls';
  controls.append(panel.headphoneRow, panel.returnGroup, panel.channelGroup, panel.levelGroup);

  // --- the column collapses, exactly as the application's does ------------
  //
  // Same classes, same CSS (this view carries .view-home for that reason):
  // a header with the fold button, a strip with the unfold button that is the
  // only thing laid out while folded, and the picture takes the space — the
  // ResizeObserver on the tile re-places the SRT overlay by itself. The
  // choice is remembered per machine (prefs.js), because a monitor that is
  // restarted for a kick should come back the way it was left.
  const railHeader = document.createElement('div');
  railHeader.className = 'rail-header';
  const railTitle = document.createElement('span');
  railTitle.className = 'rail-title';
  railTitle.textContent = 'PICTURE & RETURN';
  const railCollapse = document.createElement('button');
  railCollapse.type = 'button';
  railCollapse.className = 'btn btn-ghost rail-collapse';
  railCollapse.textContent = '›';
  railCollapse.title = 'Hide the controls (the picture gets the space)';
  railCollapse.setAttribute('aria-label', 'Hide the controls');
  railHeader.append(railTitle, railCollapse);

  const railStrip = document.createElement('div');
  railStrip.className = 'rail-strip';
  const railExpand = document.createElement('button');
  railExpand.type = 'button';
  railExpand.className = 'btn btn-ghost rail-expand';
  railExpand.textContent = '‹';
  railExpand.title = 'Show the controls';
  railExpand.setAttribute('aria-label', 'Show the controls');
  const railStripLabel = document.createElement('span');
  railStripLabel.className = 'rail-strip-label';
  railStripLabel.textContent = 'CONTROLS';
  railStrip.append(railExpand, railStripCount, railStripLabel);

  const rail = document.createElement('aside');
  rail.className = 'home-rail monitor-rail';
  rail.setAttribute('aria-label', 'Picture and return audio');
  rail.append(
    railHeader,
    alertsRegion,
    makeRailSection('Picture', panel.pictureGroup),
    makeRailSection('Return audio', controls),
    railStrip,
  );

  let railCollapsed = readPref(PREF_MONITOR_RAIL_COLLAPSED) === true;
  function renderRail() {
    el.classList.toggle('home-rail-collapsed', railCollapsed);
    rail.setAttribute('aria-expanded', railCollapsed ? 'false' : 'true');
  }
  railCollapse.addEventListener('click', () => {
    railCollapsed = true;
    writePref(PREF_MONITOR_RAIL_COLLAPSED, true);
    renderRail();
  });
  railExpand.addEventListener('click', () => {
    railCollapsed = false;
    writePref(PREF_MONITOR_RAIL_COLLAPSED, false);
    renderRail();
  });
  renderRail();

  const homeBody = document.createElement('div');
  homeBody.className = 'home-body';
  homeBody.append(homeMain, rail);

  const audioEl = document.createElement('audio');
  audioEl.autoplay = true;
  audioEl.hidden = true;

  el.append(header, homeBody, audioEl);
  renderAlerts();

  const noop = () => {};

  return {
    el,
    videoEl: panel.videoEl,
    audioEl,
    pictureEl: panel.tileEl,
    previewEl: null,
    lamps,
    setDevBadge(visible) {
      devBadge.hidden = !visible;
    },
    setRemoteClients: noop,
    setTile: panel.setTile,
    setInputDevices: noop,
    setHeadphoneDevices: panel.setHeadphoneDevices,
    setReturnMid: panel.setReturnMid,
    setReturnChannel: panel.setReturnChannel,
    setPictureSource: panel.setPictureSource,
    setPictureAvailable: panel.setPictureAvailable,
    setPictureState: panel.setPictureState,
    setPictureOverlaid: panel.setPictureOverlaid,
    measurePictureRect: panel.measurePictureRect,
    setPreviewReserved: noop,
    setPreviewCaption: noop,
    measurePreviewRect: () => null,
    setLevels: panel.setLevels,
    setLevel: panel.setLevel,
    setPresets: noop,
    setActivePreset: noop,
    setRunning: noop,
    setBusy: noop,
    setMuteReadout: noop,
    setCoughMode: noop,
    setMonitorState: noop,
    showError,
    showNote,
    clearError,
    clearErrorIf,
  };
}
