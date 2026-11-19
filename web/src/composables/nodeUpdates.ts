import type { NodeInfo, UpdateRun } from './nodes.ts'

export function runSummary(run: UpdateRun) {
  return {
    done: run.nodes.filter((n) => n.status === 'done').length,
    total: run.nodes.length,
    current: run.nodes.find((n) => n.status === 'updating')?.name ?? null,
  }
}

export const selectable = (n: NodeInfo, runActive: boolean) => n.updatable && !runActive

export const vmImpact = (n: NodeInfo): 'pause' | 'restart' => (n.updatePausesOnly ? 'pause' : 'restart')
