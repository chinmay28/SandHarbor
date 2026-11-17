/* Wires the phone's Back button (inside the Android app) to the page. What
   a press does is decided by backStep in backButton.js. */

import { useEffect, useRef } from 'react'
import { nativeApp } from './nativeApp'
import { backStep } from './backButton'

/* Every dialog here (Modal, ActionSheet, the image viewer) is marked so and
   closes on Escape, the innermost first — which is the order Back wants. */
function dialogOpen(doc = document) {
  return doc.querySelector('[role="dialog"][aria-modal="true"]') !== null
}

function pressEscape() {
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
}

/* Hands the app's Back button to the page while it is mounted. `drawerOpen`
   and `closeDrawer` are for the accounts drawer on a phone, which is not a
   dialog but covers everything as if it were. */
export function useNativeBack(nav, { drawerOpen = false, closeDrawer } = {}) {
  // Read at the moment Back is pressed, not when the handler was installed.
  const latest = useRef(null)
  latest.current = { nav, drawerOpen, closeDrawer }

  useEffect(() => {
    const app = nativeApp()
    if (!app || typeof app.onBack !== 'function') return undefined
    return app.onBack(() => {
      const { nav: n, drawerOpen: drawer, closeDrawer: close } = latest.current
      const step = backStep({ dialogOpen: dialogOpen(), drawerOpen: drawer && Boolean(close), canBack: n.canBack, canUp: n.canUp })
      switch (step) {
        case 'dialog': pressEscape(); return true
        case 'drawer': close(); return true
        case 'back': n.back(); return true
        case 'up': n.up(); return true
        default: return false
      }
    })
  }, [])
}
