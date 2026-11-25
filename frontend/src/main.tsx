import { StrictMode, Profiler } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import { i18nReady } from './i18n'
import App from './App.tsx'

// Only English is bundled (#3175). Wait for the language in use, so the first
// paint is not English or raw keys; a slow network gets English after 3 s and
// switches when the file arrives.
const LANGUAGE_WAIT_MS = 3000

Promise.race([i18nReady, new Promise((resolve) => setTimeout(resolve, LANGUAGE_WAIT_MS))])
  .catch(() => {})
  .then(() => {
    const diagnostics = import.meta.env.DEV ? window.bambasticDiagnostics : undefined;
    createRoot(document.getElementById('root')!, diagnostics ? {
      onCaughtError: (error, info) => diagnostics.crash('react.boundary', error, info.componentStack),
      onUncaughtError: (error, info) => diagnostics.crash('react.uncaught', error, info.componentStack),
      onRecoverableError: (error, info) => diagnostics.crash('react.recoverable', error, info.componentStack),
    } : undefined).render(
      <StrictMode>
        {diagnostics ? <Profiler id="App" onRender={(id, phase, duration) =>
          diagnostics.event('react.commit', { id, phase, ms: Math.round(duration) })}><App /></Profiler> : <App />}
      </StrictMode>,
    )
  })
