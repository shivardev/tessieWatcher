import { describe, expect, it } from 'vitest'
import { withBrowserLocaltime } from './database'

describe('localtime for server queries', () => {
  const at = (offsetMinutes: number) => ({ getTimezoneOffset: () => offsetMinutes }) as Date

  it("replaces SQLite's 'localtime' with the browser's UTC offset", () => {
    expect(withBrowserLocaltime("SELECT datetime(start_time, 'localtime') FROM drives", at(240))).toBe(
      "SELECT datetime(start_time, '-240 minutes') FROM drives",
    )
    expect(withBrowserLocaltime("date(x, 'LOCALTIME')", at(-330))).toBe("date(x, '+330 minutes')")
  })

  it('leaves queries without it untouched', () => {
    expect(withBrowserLocaltime("SELECT 'local' AS a", at(0))).toBe("SELECT 'local' AS a")
  })
})
