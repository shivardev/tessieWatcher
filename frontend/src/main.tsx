import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { CloudProbe } from './CloudProbe.tsx'

// ?cloud opens the Layerbase query proof-of-concept instead of the normal
// viewer; everything else is unchanged.
const cloud = new URLSearchParams(globalThis.location?.search).has('cloud')

createRoot(document.getElementById('root')!).render(
  <StrictMode>{cloud ? <CloudProbe /> : <App />}</StrictMode>,
)
