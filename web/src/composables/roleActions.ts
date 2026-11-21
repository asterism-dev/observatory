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
