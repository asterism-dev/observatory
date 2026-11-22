export function actionSuggestions(all: string[]): string[] {
  const groups = new Set<string>()
  for (const a of all) {
    const [service, resource] = a.split('.')
    groups.add(`${service}.*`)
    if (resource) groups.add(`${service}.${resource}.*`)
  }
  return ['*', ...groups, ...new Set(all)]
}

export function unknownEffects(rules: { effect: string }[]): string[] {
  return [...new Set(rules.map((r) => r.effect).filter((e) => e !== 'allow' && e !== 'deny'))]
}

export function unknownMarks(rules: { action: string }[], unknown: string[]): boolean[] {
  return rules.map((r) => unknown.includes(r.action))
}

export function actionHints(entries: { action: string; description: string }[]): Map<string, string> {
  return new Map(entries.filter((e) => e.description).map((e) => [e.action, e.description]))
}
