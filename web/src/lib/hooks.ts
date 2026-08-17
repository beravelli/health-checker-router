import { useCallback, useEffect, useRef, useState } from 'react'

/**
 * useAsync loads data once and exposes a reload function.
 *
 * There is deliberately no polling here — live updates arrive over SSE
 * (see useEventStream), and layering polling on top would double the load for
 * no extra freshness.
 */
export function useAsync<T>(load: () => Promise<T>, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<Error | null>(null)
  const [loading, setLoading] = useState(true)

  // Guards against a slow first request resolving after a newer one.
  const generation = useRef(0)

  const reload = useCallback(async () => {
    const gen = ++generation.current
    setLoading(true)
    try {
      const result = await load()
      if (gen === generation.current) {
        setData(result)
        setError(null)
      }
    } catch (err) {
      if (gen === generation.current) {
        setError(err instanceof Error ? err : new Error(String(err)))
      }
    } finally {
      if (gen === generation.current) setLoading(false)
    }
    // load is intentionally excluded: callers pass inline closures.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  useEffect(() => {
    void reload()
  }, [reload])

  return { data, error, loading, reload }
}

export type StreamEvent = {
  type: 'health' | 'weights' | 'failopen' | 'incident' | 'rollout'
  at: string
  data: unknown
}

/**
 * useEventStream subscribes to the server's SSE feed.
 *
 * EventSource reconnects on its own, honouring the retry interval the server
 * sends, so there is no manual backoff here.
 */
export function useEventStream(onEvent: (e: StreamEvent) => void, enabled = true) {
  const [connected, setConnected] = useState(false)

  // Held in a ref so re-renders don't tear down the connection.
  const handler = useRef(onEvent)
  handler.current = onEvent

  useEffect(() => {
    if (!enabled) return

    const source = new EventSource('/api/events', { withCredentials: true })

    const types: StreamEvent['type'][] = ['health', 'weights', 'failopen', 'incident', 'rollout']
    const listeners = types.map((type) => {
      const listener = (ev: MessageEvent) => {
        try {
          handler.current(JSON.parse(ev.data) as StreamEvent)
        } catch {
          // A malformed frame should not kill the stream.
        }
      }
      source.addEventListener(type, listener as EventListener)
      return { type, listener }
    })

    source.onopen = () => setConnected(true)
    source.onerror = () => setConnected(false)

    return () => {
      listeners.forEach(({ type, listener }) =>
        source.removeEventListener(type, listener as EventListener),
      )
      source.close()
    }
  }, [enabled])

  return connected
}

/** useLocalState persists a UI preference across reloads. */
export function useLocalState<T extends string>(key: string, initial: T) {
  const [value, setValue] = useState<T>(() => {
    try {
      return (window.localStorage.getItem(key) as T) ?? initial
    } catch {
      return initial
    }
  })

  const set = useCallback(
    (next: T) => {
      setValue(next)
      try {
        window.localStorage.setItem(key, next)
      } catch {
        // Private browsing or a full quota — the preference just won't stick.
      }
    },
    [key],
  )

  return [value, set] as const
}
