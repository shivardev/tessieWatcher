// One-shot "link it up" helper. Run:  node cloud-link.mjs
//
// Reads your Layerbase credentials from the local file (the key never
// leaves your machine or enters any chat), confirms the key authenticates,
// and if it does, pushes tesla-live.db up to the cloud via teslalog.exe.
// If the key is rejected it stops at the preflight with the exact error.
import { readFile } from 'node:fs/promises'
import { spawn } from 'node:child_process'

const CREDS = process.argv[2] ?? 'C:/Users/konda/Downloads/teslamate-credentials.txt'
const DB = 'G:/coding/tessieWatcher/tesla-live.db'
const EXE = 'G:/coding/tessieWatcher/teslalog.exe'

const text = await readFile(CREDS, 'utf8').catch(() => '')
if (!text) {
  console.error(`Could not read credentials file: ${CREDS}`)
  process.exit(1)
}

const key = text.match(/sk_[A-Za-z0-9_]+/)?.[0]

// The authoritative source of host + UUID is the endpoint line itself:
//   POST https://<host>/v1/databases/<uuid>/query
// A key is scoped to its database ON ITS CLUSTER host, so we must use the
// exact host printed for this database (e.g. sage.cloud.layerbase.dev),
// not a generic cloud.layerbase.dev - hitting the wrong cluster returns
// "Invalid API key".
const endpoint = text.match(
  /(https?:\/\/[^/\s"'`]+)\/v1\/databases\/([0-9a-fA-F-]{36})/,
)
const looseUuid = text.match(
  /[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/,
)?.[0]
const base = (endpoint?.[1] ?? 'https://sage.cloud.layerbase.dev').replace(/\/+$/, '')
const dbId = endpoint?.[2] ?? looseUuid ?? '9261e04e-421b-4062-a7a4-2bd7f0488b6f'

if (!key) {
  console.error('No sk_... key found in the credentials file.')
  process.exit(1)
}
console.log(`key: sk_…(${key.length} chars)   db: ${dbId}   base: ${base}`)

console.log('\n[1/2] Testing the key (SELECT 1) …')
let res
try {
  res = await fetch(`${base}/v1/databases/${dbId}/query`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ query: 'SELECT 1 AS n' }),
  })
} catch (e) {
  console.error('Network error reaching Layerbase:', e.message)
  process.exit(1)
}
const body = await res.text()
console.log(`  HTTP ${res.status}: ${body.slice(0, 300)}`)
if (!res.ok || /"error"/.test(body)) {
  console.error('\n✗ The key did not authenticate. Nothing was pushed. This is Layerbase rejecting the key,')
  console.error('  independent of teslalog — the same thing a plain curl shows. Fix the key/DB in Layerbase first.')
  process.exit(1)
}
console.log('  ✓ authenticated.')

console.log('\n[2/2] Pushing tesla-live.db to the cloud …')
const child = spawn(EXE, ['cloud-push', '-url', base, '-id', dbId, '-db', DB], {
  stdio: 'inherit',
  env: { ...process.env, LAYERBASE_API_KEY: key },
})
child.on('exit', (code) => {
  if (code === 0) {
    console.log('\n✓ Linked up. Your data is in the cloud.')
    console.log('  Read side: open the viewer at  http://localhost:5173/?cloud  and paste the same key.')
  } else {
    console.error(`\n✗ cloud-push exited with code ${code}.`)
  }
  process.exit(code ?? 1)
})
