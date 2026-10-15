import { describe, expect, it } from 'vitest'
import { selectRange } from './useRowSelection'

describe('row selection ranges', () => {
  const order = [10, 11, 12, 13, 14]

  it('selects every row between the anchor and the shift-clicked row, either direction', () => {
    expect([...selectRange(new Set([11]), order, 11, 13, true)].sort()).toEqual([11, 12, 13])
    expect([...selectRange(new Set(), order, 14, 12, true)].sort()).toEqual([12, 13, 14])
  })

  it('clears a range when the clicked row is being unticked', () => {
    expect([...selectRange(new Set(order), order, 11, 13, false)].sort()).toEqual([10, 14])
  })

  it('keeps selected rows outside the shown order untouched', () => {
    expect([...selectRange(new Set([99]), order, 10, 11, true)].sort((a, b) => a - b)).toEqual([10, 11, 99])
  })

  it('falls back to a single row when the anchor is not shown', () => {
    expect([...selectRange(new Set(), order, 42, 12, true)]).toEqual([12])
  })
})
