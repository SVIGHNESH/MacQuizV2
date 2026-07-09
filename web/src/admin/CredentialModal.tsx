import { useState } from 'react'

/**
 * The one-time credential, shown exactly once (docs/11 "Users + provision").
 * The API never returns it again, so this modal is the only place it exists -
 * dismissing it is the point of no return, hence the ink button.
 */
export default function CredentialModal({
  fullName,
  password,
  onDismiss,
}: {
  fullName: string
  password: string
  onDismiss: () => void
}) {
  const [copied, setCopied] = useState(false)

  // navigator.clipboard is undefined on insecure origins and can reject when
  // the document is not focused; the credential stays selectable either way.
  const copy = () => {
    navigator.clipboard
      ?.writeText(password)
      .then(() => setCopied(true))
      .catch(() => setCopied(false))
  }

  return (
    <div className="modal-overlay" role="alertdialog" aria-modal="true">
      <div className="modal-panel admin-credential-reveal">
        <span className="chip chip-lifecycle chip-lifecycle-submitted">
          <span className="chip-dot" aria-hidden="true" />
          User provisioned
        </span>
        <h2 className="credential-title">{fullName} is ready</h2>
        <p className="body-copy">
          Copy the one-time credential now - it is shown{' '}
          <strong>exactly once</strong> and cannot be retrieved later.
        </p>

        <div className="credential-well">
          <span className="eyebrow">One-time credential</span>
          <div className="credential-row">
            <code className="admin-credential-value">{password}</code>
            <button className="button button-small button-quiet" type="button" onClick={copy}>
              {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
        </div>

        <p className="credential-notice">
          <span aria-hidden="true">!</span>
          <span>{fullName} must reset this password on first login.</span>
        </p>

        <button
          className="button button-commit credential-done"
          type="button"
          onClick={onDismiss}
        >
          I've saved it - done
        </button>
      </div>
    </div>
  )
}
