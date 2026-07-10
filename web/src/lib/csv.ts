/** RFC 4180: quote a field only when it carries a delimiter, quote, or newline. */
export function csvField(value: string | number): string {
  const text = String(value)
  return /[",\r\n]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text
}

/**
 * Serialize rows and hand them to the browser as a named download. Pure
 * client-side - the data is already in hand, so no endpoint exists and none
 * is needed. CRLF line endings keep Excel happy.
 */
export function downloadCsv(
  filename: string,
  rows: ReadonlyArray<ReadonlyArray<string | number>>,
): void {
  const text = rows.map((cells) => cells.map(csvField).join(',')).join('\r\n')
  const blob = new Blob([text], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.append(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
}
