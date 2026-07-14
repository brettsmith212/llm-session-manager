/**
 * Keep llmux's tmux state in sync with Amp and expose the llmux control room
 * in Amp's command palette.
 */
import type { PluginAPI, ThreadID, ThreadState } from '@ampcode/plugin'

type LlmuxState = 'working' | 'waiting' | 'idle'

export default function (amp: PluginAPI) {
  const threadStates = new Map<ThreadID, ThreadState>()
  const subscribed = new Set<ThreadID>()
  let lastPublished: LlmuxState | null = null
  let publishing = false

  function aggregate(): LlmuxState {
    let working = false
    for (const state of threadStates.values()) {
      if (state === 'awaiting-approval' || state === 'error') return 'waiting'
      if (state === 'running') working = true
    }
    return working ? 'working' : 'idle'
  }

  async function publish(): Promise<void> {
    if (publishing) return
    publishing = true
    try {
      while (true) {
        const next = aggregate()
        if (next === lastPublished) return
        try {
          await amp.$`llmux state ${next}`
          lastPublished = next
        } catch (error) {
          amp.logger.log('Unable to update llmux state:', error)
          return
        }
      }
    } finally {
      publishing = false
    }
  }

  amp.on('session.start', async (_event, ctx) => {
    const thread = ctx.thread
    if (subscribed.has(thread.id)) return
    subscribed.add(thread.id)

    let stateVersion = 0
    thread.state.subscribe((state: ThreadState) => {
      stateVersion++
      threadStates.set(thread.id, state)
      void publish()
    })

    // Subscribe first so no transition can be missed, then reconcile with a
    // snapshot. Amp can emit a transient startup state during subscribe; that
    // must not prevent a subsequent get() from settling llmux back to idle.
    // Conversely, an event emitted while get() is in flight is newer than the
    // snapshot and must win.
    const versionBeforeGet = stateVersion
    try {
      const current = await thread.state.get()
      if (stateVersion === versionBeforeGet) {
        threadStates.set(thread.id, current)
        await publish()
      }
    } catch (error) {
      amp.logger.log('Unable to read Amp thread state:', error)
    }
  })

  amp.registerCommand(
    'llmux-open-picker',
    {
      title: 'Open agent control room',
      category: 'llmux',
      description: 'Monitor and switch managed LLM sessions in tmux.',
      availability: process.env.TMUX_PANE
        ? { type: 'enabled' }
        : { type: 'disabled', reason: 'Amp is not running inside tmux' },
    },
    async (ctx) => {
      try {
        await ctx.$`llmux list`
      } catch (error) {
        amp.logger.log('Unable to open llmux control room:', error)
        await ctx.ui.notify('Could not open the llmux agent control room.')
      }
    },
  )

  // A fresh interactive Amp can be waiting at the prompt before it has a
  // thread, so session.start may not fire until the first message. Clear
  // llmux's initial "starting" state as soon as the plugin itself is ready.
  void publish()
}
