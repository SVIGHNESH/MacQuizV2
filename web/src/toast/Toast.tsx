import { useCallback, useRef, useState, type ReactNode } from 'react'
import { ToastContext, type ToastKind } from './context'

// docs/11 section 4: auto-dismiss 3.2s.
const TOAST_DURATION_MS = 3_200

interface ToastEntry {
  id: number
  message: string
  kind: ToastKind
}

/**
 * The one sanctioned toast style (docs/11 section 4): ink surface, a
 * semantic dot, top-center, auto-dismiss. Only one toast shows at a time -
 * a newer call replaces whatever is currently up rather than stacking.
 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toast, setToast] = useState<ToastEntry | null>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const nextId = useRef(0)

  const showToast = useCallback((message: string, kind: ToastKind = 'info') => {
    if (timer.current) clearTimeout(timer.current)
    const id = ++nextId.current
    setToast({ id, message, kind })
    timer.current = setTimeout(() => {
      setToast((current) => (current?.id === id ? null : current))
    }, TOAST_DURATION_MS)
  }, [])

  return (
    <ToastContext.Provider value={{ showToast }}>
      {children}
      {toast && (
        <div className="toast-viewport">
          <p className={`toast toast-${toast.kind}`} role="status">
            <span className="toast-dot" aria-hidden="true" />
            {toast.message}
          </p>
        </div>
      )}
    </ToastContext.Provider>
  )
}
