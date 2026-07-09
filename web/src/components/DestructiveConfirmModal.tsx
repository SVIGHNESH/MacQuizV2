import { useState, type FormEvent } from 'react'

/**
 * The documented two-step destructive flow (docs/11 section 4 "Modal
 * (destructive flow)"): required reason first, then a confirm step that
 * restates the consequence in a danger-tint card and only shows the red
 * button once the reason is committed. Shared by every reason-gated
 * destructive action (kick, readmit, score override).
 */
export default function DestructiveConfirmModal({
  title,
  subtitle,
  consequence,
  reasonLabel,
  confirmLabel,
  busy = false,
  error,
  onCancel,
  onConfirm,
}: {
  title: string
  subtitle?: string
  consequence: string
  reasonLabel: string
  confirmLabel: string
  busy?: boolean
  error?: string | null
  onCancel: () => void
  onConfirm: (reason: string) => void
}) {
  const [step, setStep] = useState<'reason' | 'confirm'>('reason')
  const [reason, setReason] = useState('')

  const submitReason = (e: FormEvent) => {
    e.preventDefault()
    if (!reason.trim()) return
    setStep('confirm')
  }

  return (
    <div className="modal-overlay" role="presentation" onClick={onCancel}>
      <div
        className="modal-panel"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
      >
        <h2 className="modal-title">{title}</h2>
        {subtitle && <p className="modal-subtitle">{subtitle}</p>}

        {step === 'reason' ? (
          <form className="form" onSubmit={submitReason}>
            <div className="field">
              <label className="field-label" htmlFor="destructive-reason">
                {reasonLabel}
              </label>
              <textarea
                id="destructive-reason"
                className="input"
                rows={3}
                autoFocus
                required
                value={reason}
                onChange={(e) => setReason(e.target.value)}
              />
            </div>
            <div className="modal-actions">
              <button type="button" className="button button-quiet" onClick={onCancel}>
                Cancel
              </button>
              <button type="submit" className="button button-primary" disabled={!reason.trim()}>
                Continue
              </button>
            </div>
          </form>
        ) : (
          <div className="form">
            <p className="modal-consequence">{consequence}</p>
            {error && <p className="form-error">{error}</p>}
            <div className="modal-actions">
              <button
                type="button"
                className="button button-quiet"
                disabled={busy}
                onClick={() => setStep('reason')}
              >
                Back
              </button>
              <button
                type="button"
                className="button button-danger"
                disabled={busy}
                onClick={() => onConfirm(reason.trim())}
              >
                {confirmLabel}
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
