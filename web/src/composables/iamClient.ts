import { Code, ConnectError, createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'

import { IAMService } from '@/gen/observatory/iam/v1/iam_pb'

export const iamClient = createClient(
  IAMService,
  createConnectTransport({
    baseUrl: '/api/iam/v1',
    fetch: (input, init) => fetch(input, { ...init, credentials: 'include' }),
    // Connect unary calls are POSTs, which the edge refuses without the CSRF header.
    interceptors: [
      (next) => (req) => {
        req.header.set('X-Observatory', '1')
        return next(req)
      },
    ],
  }),
)

export const isUnauthenticated = (e: unknown) => e instanceof ConnectError && e.code === Code.Unauthenticated
export const isNotFound = (e: unknown) => e instanceof ConnectError && e.code === Code.NotFound
export const errorMessage = (e: unknown) => (e instanceof ConnectError ? e.rawMessage : e instanceof Error ? e.message : String(e))
