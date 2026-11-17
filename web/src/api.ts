export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { 'X-Observatory': '1', 'Content-Type': 'application/json', ...init.headers },
  })
  if (!res.ok) throw new ApiError(res.status, errorMessage(await res.text()) || res.statusText)
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

function errorMessage(body: string): string {
  try {
    const parsed: unknown = JSON.parse(body)
    if (parsed && typeof parsed === 'object' && 'error' in parsed && typeof parsed.error === 'string') return parsed.error
  } catch {
    // not JSON, show the body as is
  }
  return body
}
