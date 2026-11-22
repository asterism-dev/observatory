import { createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'

import { QuotaService } from '@/gen/observatory/quota/v1/quota_pb'

export const quotaClient = createClient(
  QuotaService,
  createConnectTransport({
    baseUrl: '/api/quota/v1',
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
