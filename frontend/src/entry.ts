// Vite uses this entry in development so boot failures are captured before React mounts.
import { installDiagnostics } from './diagnostics/browser';

installDiagnostics();
void import('./main').catch(error => {
  window.bambasticDiagnostics?.crash('app.start', error);
  const root = document.getElementById('root');
  if (!root) return;
  const message = document.createElement('p');
  message.textContent = 'Bambastic could not load the application.';
  const retry = document.createElement('button');
  retry.textContent = 'Retry';
  retry.onclick = () => location.reload();
  root.replaceChildren(message, retry);
});
