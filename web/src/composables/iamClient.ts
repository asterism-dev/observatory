import { createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'

import { errorKind } from '@/composables/apiError'
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

export { errorText as errorMessage } from '@/composables/apiError'
export const isUnauthenticated = (e: unknown) => errorKind(e) === 'unauthenticated'
export const isNotFound = (e: unknown) => errorKind(e) === 'notFound'
