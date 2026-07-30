// Entry point referenced by index.html.
//
// Owner: WP-5b.
//
// Kept deliberately thin: the shell lives in ./ui/app.js, the styling in
// ./styles/main.css, and the monitor is reached only from inside ./ui/app.js
// through WP-5a's createMonitor factory in ./monitor/monitor.js — this file
// never imports the monitor module directly.
//
// One window, no router, no menu (specification section 10).
import './styles/main.css';
import { mountApp } from './ui/app.js';

const root = document.querySelector('#app');
mountApp(root);
