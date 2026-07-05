import { useEffect, useRef, useState } from 'react'

export type SaveState =
  | { phase: 'saved' }
  | { phase: 'pending' }
  | { phase: 'saving' }
  | { phase: 'error'; message: string; fields?: Record<string, string> }

export type SaveResult =
  | { ok: true }
  | { ok: false; message: string; fields?: Record<string, string> }

export const AUTOSAVE_DELAY_MS = 700

/**
 * Debounced whole-value autosave. Every edit restarts the timer; a result
 * that arrives after a newer edit is discarded, so the indicator never
 * regresses to "saved" while changes are still outstanding and the latest
 * value is always the one that lands.
 */
export function useAutosave<T>(
  value: T,
  save: (value: T) => Promise<SaveResult>,
  delayMs: number = AUTOSAVE_DELAY_MS,
): SaveState {
  const [state, setState] = useState<SaveState>({ phase: 'saved' })
  const seq = useRef(0)
  const mounted = useRef(false)

  useEffect(() => {
    if (!mounted.current) {
      mounted.current = true
      return
    }
    const mySeq = ++seq.current
    setState({ phase: 'pending' })
    const timer = setTimeout(() => {
      if (mySeq !== seq.current) return
      setState({ phase: 'saving' })
      save(value).then(
        (result) => {
          if (mySeq !== seq.current) return
          setState(
            result.ok
              ? { phase: 'saved' }
              : {
                  phase: 'error',
                  message: result.message,
                  fields: result.fields,
                },
          )
        },
        () => {
          if (mySeq !== seq.current) return
          setState({
            phase: 'error',
            message: 'Could not reach the server. Your latest edits are not saved yet.',
          })
        },
      )
    }, delayMs)
    return () => clearTimeout(timer)
    // Re-running on `save` identity changes would restart the debounce on
    // every render; callers pass a stable callback instead.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value, delayMs])

  return state
}
