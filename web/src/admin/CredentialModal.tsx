import { useRef, useState } from 'react'

/**
 * Copy without the async Clipboard API, for origins that do not have it.
 * Returns whether the copy actually happened.
 *
 * Must stay synchronous: execCommand is gated on the click's transient user
 * activation, and awaiting anything first spends it, so this can never be
 * reached from after an `await`.
 */
function copyBySelection(text: string): boolean {
  const scratch = document.createElement('textarea')
  scratch.value = text
  // Off-screen rather than hidden: display:none and visibility:hidden are not
  // selectable, and a focused element scrolls into view if it is in flow.
  scratch.setAttribute('readonly', '')
  scratch.style.cssText = 'position:fixed;top:0;left:0;width:1px;height:1px;opacity:0;'
  document.body.appendChild(scratch)
  try {
    scratch.select()
    scratch.setSelectionRange(0, text.length)
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    scratch.remove()
  }
}

/** Leaves the credential selected so the reader can finish the job with Ctrl+C. */
function selectNode(node: HTMLElement | null) {
  if (!node) return
  const range = document.createRange()
  range.selectNodeContents(node)
  const selection = window.getSelection()
  selection?.removeAllRanges()
  selection?.addRange(range)
}

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
  const [copyState, setCopyState] = useState<'idle' | 'copied' | 'manual'>('idle')
  const valueRef = useRef<HTMLElement>(null)

  /**
   * navigator.clipboard exists only in a secure context, so it is simply
   * absent when the app is served over plain HTTP - and the previous
   * `navigator.clipboard?.writeText(...)` then evaluated to undefined and the
   * button did nothing at all, with no error and no feedback. This credential
   * is shown exactly once, so a Copy button that silently fails is the worst
   * thing on this screen: fall back to the legacy path, and if even that
   * fails, say so and leave the text selected rather than pretending.
   */
  const copy = () => {
    if (navigator.clipboard?.writeText) {
      navigator.clipboard
        .writeText(password)
        .then(() => setCopyState('copied'))
        .catch(() => {
          // No second attempt via execCommand here: the await has already
          // spent the click's user activation, so it would fail too.
          selectNode(valueRef.current)
          setCopyState('manual')
        })
      return
    }
    if (copyBySelection(password)) {
      setCopyState('copied')
      return
    }
    selectNode(valueRef.current)
    setCopyState('manual')
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
            <code className="admin-credential-value" ref={valueRef}>
              {password}
            </code>
            <button className="button button-small button-quiet" type="button" onClick={copy}>
              {copyState === 'copied' ? 'Copied' : 'Copy'}
            </button>
          </div>
          {copyState === 'manual' ? (
            <p className="credential-copy-hint" role="status">
              This browser blocked the copy. The credential is selected - press
              Ctrl+C (Cmd+C on a Mac) to copy it.
            </p>
          ) : null}
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
