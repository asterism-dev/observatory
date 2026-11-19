export const DISMISS_KEY = 'obs-dismissed-release'

export const releaseURL = (tag: string) => `https://github.com/asterism-dev/observatory/releases/tag/${tag}`

// The hub sends latestVersion only when it is newer than itself.
export function showBanner(me: { user: { platformAdmin: boolean }; latestVersion: string } | null, dismissed: string | null): boolean {
  return !!me && me.user.platformAdmin && me.latestVersion !== '' && me.latestVersion !== dismissed
}
