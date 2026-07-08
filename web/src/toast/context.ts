import { createContext, useContext } from 'react'

// docs/11-frontend-design-system.md section 4 "Toast": one style, ink
// surface, semantic 8px dot, top-center, auto-dismiss 3.2s. The dot color is
// the only thing that varies per call site.
export type ToastKind = 'success' | 'info' | 'warning' | 'danger'

export interface ToastContextValue {
  showToast: (message: string, kind?: ToastKind) => void
}

export const ToastContext = createContext<ToastContextValue | null>(null)

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (!ctx) throw new Error('useToast must be used inside <ToastProvider>')
  return ctx
}
