import { Code, ConnectError } from '@connectrpc/connect'

// Duck-typed so node tests need not load api.ts, whose parameter properties they cannot strip.
const restStatus = (e: unknown) => (e instanceof Error && 'status' in e && typeof e.status === 'number' ? e.status : 0)

export type ErrorKind = 'unauthenticated' | 'forbidden' | 'notFound' | 'other'

export function errorKind(e: unknown): ErrorKind {
  if (e instanceof ConnectError) {
    if (e.code === Code.Unauthenticated) return 'unauthenticated'
    if (e.code === Code.PermissionDenied) return 'forbidden'
    if (e.code === Code.NotFound) return 'notFound'
  }
  const status = restStatus(e)
  if (status === 401) return 'unauthenticated'
  if (status === 403) return 'forbidden'
  if (status === 404) return 'notFound'
  return 'other'
}

export const errorText = (e: unknown) => (e instanceof ConnectError ? e.rawMessage : e instanceof Error ? e.message : String(e))
