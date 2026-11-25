export class RequestError extends Error {
  constructor(
    readonly status: number,
    detail: string,
  ) {
    super(detail);
  }
}

export async function request(
  path: string,
  options: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(options.headers);
  const credential = sessionStorage.getItem('bambastic.session');
  if (credential) headers.set('Authorization', 'Bearer ' + credential);
  const response = await fetch(path, {
    ...options,
    headers,
    credentials: 'same-origin',
  });
  if (!response.ok) {
    const failure = await response.json().catch(() => ({}));
    throw new RequestError(
      response.status,
      failure.detail || failure.error || response.statusText,
    );
  }
  return response;
}

export async function read<T>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const response = await request(path, options);
  return response.status === 204 ? (undefined as T) : response.json();
}

export function submit<T>(path: string, body: unknown) {
  return read<T>(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

export const message = (error: unknown) =>
  error instanceof Error ? error.message : 'Request failed';
