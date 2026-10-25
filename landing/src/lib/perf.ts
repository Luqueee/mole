// Performance helpers for the animated parts of the site.
//
// Nobody should pay for a hero with their battery or a stuttering page. These
// helpers decide how much to draw (a quality tier), watch the real frame rate
// and step the quality down by themselves, and stop all work when the animation
// is off screen or the tab is hidden.

/** 0 = lean, 1 = balanced, 2 = full. */
export type Tier = 0 | 1 | 2

export const prefersCalm = () =>
  matchMedia("(prefers-reduced-motion: reduce)").matches

/** A first guess from what the browser tells us about the machine. */
export function initialTier(): Tier {
  const nav = navigator as Navigator & {
    deviceMemory?: number
    connection?: { saveData?: boolean }
  }
  if (nav.connection?.saveData) return 0
  const cores = nav.hardwareConcurrency ?? 4
  const mem = nav.deviceMemory ?? 8
  let tier: Tier = 2
  if (cores <= 4 || mem <= 4) tier = 1
  if (cores <= 2 || mem <= 2) tier = 0
  // phones and tablets: never the full thing
  if (matchMedia("(pointer: coarse)").matches) tier = Math.min(tier, 1) as Tier
  return tier
}

/** Device pixel ratio worth paying for at a tier. */
export const dprFor = (tier: Tier) =>
  Math.min(devicePixelRatio || 1, [1, 1.5, 2][tier])

type LoopOptions = {
  tier: Tier
  /** called when the frame rate forced the quality down */
  onTier?: (tier: Tier) => void
  /** frame rate the animation wants right now (it can ask for less once it has settled) */
  fps?: () => number
}

/**
 * A requestAnimationFrame loop that only runs while `el` is on screen and the
 * tab is visible, caps its frame rate, and lowers the tier on its own when the
 * machine can't keep up.
 */
export function runLoop(
  el: Element,
  step: (now: number, dt: number) => void,
  o: LoopOptions,
) {
  let tier = o.tier
  let onScreen = false
  let raf = 0
  let last = 0
  let warm = 0
  let drops = 0
  let samples: number[] = []

  const frame = (now: number) => {
    raf = requestAnimationFrame(frame)
    const want = Math.min(o.fps?.() ?? 60, tier === 0 ? 30 : 60)
    if (last && now - last < 1000 / want - 2) return
    const gap = last ? now - last : 16
    last = now
    // judge the machine only while it is being asked for full speed
    if (gap < 250 && want >= 60) {
      warm += gap
      if (warm > 1400) {
        samples.push(gap)
        if (samples.length >= 40) {
          const avg = samples.reduce((a, b) => a + b, 0) / samples.length
          samples = []
          if (avg > 27 && tier > 0 && drops < 2) {
            tier = (tier - 1) as Tier
            drops++
            warm = 0
            o.onTier?.(tier)
          }
        }
      }
    }
    step(now, Math.min(0.05, gap / 1000))
  }

  const sync = () => {
    const go = onScreen && document.visibilityState === "visible"
    if (go && !raf) {
      last = 0
      raf = requestAnimationFrame(frame)
    } else if (!go && raf) {
      cancelAnimationFrame(raf)
      raf = 0
    }
  }
  new IntersectionObserver(
    (e) => {
      onScreen = e[0].isIntersecting
      sync()
    },
    { threshold: 0.02 },
  ).observe(el)
  document.addEventListener("visibilitychange", sync)

  return {
    get tier() {
      return tier
    },
  }
}
