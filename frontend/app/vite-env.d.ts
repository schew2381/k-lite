/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_KLITE_MODE?: 'mock' | 'http'
  readonly VITE_KLITE_API?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
