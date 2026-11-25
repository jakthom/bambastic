async function start() {
  if (import.meta.env.DEV) await import('./dev-events');
  await import('./main');
}
void start();
