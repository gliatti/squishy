// Minimal SSE wrapper. Emits typed callbacks.
export type StreamEvent = {
  kind: string
  level?: string
  message?: string
  data?: Record<string, unknown>
  ts: string
}

export function subscribe(runId: string, onEvent: (e: StreamEvent) => void): () => void {
  const es = new EventSource(`/api/v1/runs/${runId}/events`)
  for (const k of ['run.status', 'step.status', 'batch.progress', 'log']) {
    es.addEventListener(k, (ev: MessageEvent) => {
      try {
        const e = JSON.parse(ev.data)
        onEvent(e)
      } catch { /* ignore malformed */ }
    })
  }
  es.onerror = () => { /* EventSource retries automatically */ }
  return () => es.close()
}
