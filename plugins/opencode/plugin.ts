/**
 * OpenCode plugin shim that tracks session state (working/idle/waiting) on
 * the tmux session, for display in the picker.
 *
 * The actual state management and tmux interaction live in the Go `llmux`
 * binary. This plugin translates OpenCode's event bus into
 * `llmux state <working|waiting|idle>` calls.
 *
 * One OpenCode process can run several sessions (root chat plus sub-agents),
 * so state is aggregated across all session IDs.
 */

type State = 'working' | 'waiting' | 'idle'

async function setState($: any, state: State): Promise<void> {
  try {
    await $`llmux state ${state}`
  } catch {
    // State updates are best-effort and must never interrupt the agent.
  }
}

export const TmuxSessionManager = async ({ $ }: { $: any }) => {
  const busy = new Set<string>()
  const pending = new Map<string, Set<string>>()
  let lastPublished: State | null = null

  function addPending(sessionID: string, id: string): void {
    let requests = pending.get(sessionID)
    if (!requests) {
      requests = new Set()
      pending.set(sessionID, requests)
    }
    requests.add(id)
  }

  function removePending(sessionID: string, id: string): void {
    const requests = pending.get(sessionID)
    if (!requests) return
    requests.delete(id)
    if (requests.size === 0) pending.delete(sessionID)
  }

  function forget(sessionID: string): void {
    busy.delete(sessionID)
    pending.delete(sessionID)
  }

  function aggregate(): State {
    if (pending.size > 0) return 'waiting'
    if (busy.size > 0) return 'working'
    return 'idle'
  }

  async function publish(): Promise<void> {
    const next = aggregate()
    if (next === lastPublished) return
    await setState($, next)
    lastPublished = next
  }

  await publish()

  return {
    event: async ({ event }: { event: { type: string; properties?: any } }) => {
      const properties = event.properties ?? {}
      const sessionID: string | undefined = properties.sessionID

      switch (event.type) {
        case 'session.status': {
          if (!sessionID) break
          if (properties.status?.type === 'busy') busy.add(sessionID)
          else busy.delete(sessionID)
          await publish()
          break
        }

        case 'permission.asked':
        case 'permission.v2.asked':
        case 'question.asked': {
          if (!sessionID || properties.id == null) break
          addPending(sessionID, String(properties.id))
          await publish()
          break
        }

        case 'permission.replied':
        case 'permission.v2.replied':
        case 'question.replied':
        case 'question.rejected': {
          if (!sessionID || properties.requestID == null) break
          removePending(sessionID, String(properties.requestID))
          await publish()
          break
        }

        case 'session.deleted': {
          const id = properties.info?.id ?? sessionID
          if (id) forget(id)
          await publish()
          break
        }
      }
    },
  }
}
