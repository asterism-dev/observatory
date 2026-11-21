import { createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'

import { AuditService } from '@/gen/observatory/audit/v1/audit_pb'

export const auditClient = createClient(
  AuditService,
  createConnectTransport({
    baseUrl: '/api/audit/v1',
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
