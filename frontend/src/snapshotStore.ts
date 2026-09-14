// Local persistence for the last snapshot fetched from a live teslalog,
// so reconnecting to the same portal shows data instantly and only
// re-downloads when the portal's snapshot_revision actually changes.
//
// This is a cache, never the source of truth. Everything here is best
// effort: IndexedDB can be disabled, evicted, over quota, or throw in a
// private window, so every operation is wrapped and degrades to "no cached
// copy" rather than failing the viewer. IndexedDB is origin-scoped, so a
// snapshot saved while viewing http://pi:8083 is invisible to the hosted
// viewer on another origin - which is correct, they are different portals.
// Records are keyed by the portal's base URL so two portals don't collide.

const DB_NAME = 'teslalog-viewer'
const STORE = 'snapshots'
const DB_VERSION = 1

export type CachedSnapshot = Readonly<{ revision: string; bytes: Uint8Array }>

// A single record. bytes is stored as a Uint8Array, which the structured
// clone algorithm round-trips faithfully.
type SnapshotRecord = { url: string; revision: string; bytes: Uint8Array; savedAt: number }

const openDb = (): Promise<IDBDatabase | null> =>
  new Promise((resolve) => {
    let idb: IDBFactory | undefined
    try {
      idb = globalThis.indexedDB
    } catch {
      resolve(null) // Accessing indexedDB itself can throw in some sandboxes.
      return
    }
    if (idb === undefined) {
      resolve(null)
      return
    }
    let request: IDBOpenDBRequest
    try {
      request = idb.open(DB_NAME, DB_VERSION)
    } catch {
      resolve(null)
      return
    }
    request.onupgradeneeded = () => {
      const db = request.result
      if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE, { keyPath: 'url' })
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => resolve(null)
    request.onblocked = () => resolve(null)
  })

// loadSnapshot returns the cached snapshot for a portal, or null when
// there is none or storage is unavailable. Never rejects.
export const loadSnapshot = async (url: string): Promise<CachedSnapshot | null> => {
  const db = await openDb()
  if (db === null) return null
  try {
    return await new Promise<CachedSnapshot | null>((resolve) => {
      let request: IDBRequest<SnapshotRecord | undefined>
      try {
        request = db.transaction(STORE, 'readonly').objectStore(STORE).get(url) as IDBRequest<
          SnapshotRecord | undefined
        >
      } catch {
        resolve(null)
        return
      }
      request.onsuccess = () => {
        const record = request.result
        resolve(
          record && record.bytes instanceof Uint8Array
            ? { revision: record.revision, bytes: record.bytes }
            : null,
        )
      }
      request.onerror = () => resolve(null)
    })
  } finally {
    db.close()
  }
}

// saveSnapshot stores (or replaces) the cached snapshot for a portal.
// Never rejects: a quota or write failure just means the next connect
// re-downloads, which is the pre-cache behaviour.
export const saveSnapshot = async (
  url: string,
  revision: string,
  bytes: Uint8Array,
): Promise<void> => {
  const db = await openDb()
  if (db === null) return
  try {
    await new Promise<void>((resolve) => {
      let tx: IDBTransaction
      try {
        tx = db.transaction(STORE, 'readwrite')
      } catch {
        resolve()
        return
      }
      tx.oncomplete = () => resolve()
      tx.onerror = () => resolve()
      tx.onabort = () => resolve()
      try {
        const record: SnapshotRecord = { url, revision, bytes, savedAt: Date.now() }
        tx.objectStore(STORE).put(record)
      } catch {
        resolve()
      }
    })
  } finally {
    db.close()
  }
}
