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

async function setState($: any, state: State): Promise<boolean> {
  try {
    await $`llmux state ${state}`
    return true
  } catch {
    // State updates are best-effort and must never interrupt the agent.
    return false
  }
}

export const TmuxSessionManager = async ({ $ }: { $: any }) => {
  const active = new Set<string>()
  const failed = new Set<string>()
  const pending = new Map<string, Set<string>>()
  let lastPublished: State | null = null
  let publishing = false

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
    active.delete(sessionID)
    failed.delete(sessionID)
    pending.delete(sessionID)
  }

  function aggregate(): State {
    if (pending.size > 0 || failed.size > 0) return 'waiting'
    if (active.size > 0) return 'working'
    return 'idle'
  }

  async function publish(): Promise<void> {
    if (publishing) return
    publishing = true
    try {
      while (true) {
        const next = aggregate()
        if (next === lastPublished) return
        if (!(await setState($, next))) return
        lastPublished = next
      }
    } finally {
      publishing = false
    }
  }

  await publish()

  return {
    event: async ({ event }: { event: { type: string; properties?: any } }) => {
      const properties = event.properties ?? {}
      const sessionID: string | undefined = properties.sessionID

      switch (event.type) {
        case 'session.status': {
          if (!sessionID) break
          if (properties.status?.type === 'busy' || properties.status?.type === 'retry') {
            active.add(sessionID)
            failed.delete(sessionID)
          } else {
            active.delete(sessionID)
          }
          await publish()
          break
        }

        case 'session.error': {
          if (!sessionID) break
          active.delete(sessionID)
          failed.add(sessionID)
          await publish()
          break
        }

        // Deprecated but still emitted after session.status(idle), and useful
        // as a fallback when running against older OpenCode versions.
        case 'session.idle': {
          if (!sessionID) break
          active.delete(sessionID)
          await publish()
          break
        }

        case 'permission.asked':
        case 'permission.v2.asked':
        case 'question.asked':
        case 'question.v2.asked': {
          if (!sessionID || properties.id == null) break
          addPending(sessionID, String(properties.id))
          await publish()
          break
        }

        case 'permission.replied':
        case 'permission.v2.replied':
        case 'question.replied':
        case 'question.rejected':
        case 'question.v2.replied':
        case 'question.v2.rejected': {
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
