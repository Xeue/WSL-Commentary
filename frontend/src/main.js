// Entry point referenced by index.html.
//
// WP-0 wrote this stub so that `npm run build` succeeds before any UI exists.
// WP-5b owns it: it should import the shell from ./ui/ and the styles from
// ./styles/, and hand the monitor element to WP-5a's module in ./monitor/.
//
// The two halves of the frontend are path-disjoint:
//   src/monitor/  WP-5a  KVS viewer, mosaic crop, return audio, setSinkId
//   src/ui/       WP-5b  controls, lamps, Settings view
//   src/styles/   WP-5b

document.querySelector('#app').textContent =
  'wslcomms: frontend not implemented yet (WP-5a monitor, WP-5b shell)';
