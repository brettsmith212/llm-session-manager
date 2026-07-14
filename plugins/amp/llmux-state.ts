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

    let stateWasEmitted = false
    thread.state.subscribe((state: ThreadState) => {
      stateWasEmitted = true
      threadStates.set(thread.id, state)
      void publish()
    })

    try {
      const current = await thread.state.get()
      if (!stateWasEmitted) {
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
}
