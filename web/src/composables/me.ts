import type { GetMeResponse } from '../gen/observatory/iam/v1/iam_pb.ts'

export interface Org {
  id: string
  name: string
  personal: boolean
  role: string
}

export interface Me {
  user: { id: string; email: string; name: string; platformAdmin: boolean }
  orgs: Org[]
  docsURL: string
  version: string
  latestVersion: string
}

export interface Info {
  docsURL: string
  version: string
  latestVersion: string
}

export function toMe(res: GetMeResponse, info: Info): Me {
  const u = res.user
  return {
    user: { id: u?.id ?? '', email: u?.email ?? '', name: u?.name ?? '', platformAdmin: u?.platformAdmin ?? false },
    orgs: res.orgs.map((o) => ({ id: o.id, name: o.name, personal: o.personal, role: o.role })),
    docsURL: info.docsURL,
    version: info.version,
    latestVersion: info.latestVersion,
  }
}
