export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public code = '',
  ) {
    super(message)
  }
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { 'X-Observatory': '1', 'Content-Type': 'application/json', ...init.headers },
  })
  if (!res.ok) {
    const { error, code } = errorBody(await res.text())
    throw new ApiError(res.status, error || res.statusText, code)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

function errorBody(body: string): { error: string; code: string } {
  try {
    const parsed: unknown = JSON.parse(body)
    if (parsed && typeof parsed === 'object' && 'error' in parsed && typeof parsed.error === 'string')
      return { error: parsed.error, code: 'code' in parsed && typeof parsed.code === 'string' ? parsed.code : '' }
  } catch {
    // not JSON, show the body as is
  }
  return { error: body, code: '' }
}
