// computeLayout turns a snapshot and a width into every rectangle on the
// board, as pure math with no DOM measurement. Cards render from it and the
// dot layer reads its anchor map per frame, so both always agree.

import type { Instance, NodeObj, Service } from '@/api/types'
import { instancesByNode, pendingInstances, sortedNodes, sortedServices } from '@/store/selectors'
import type { Snapshot } from '@/store/store'

export interface Rect {
  x: number
  y: number
  w: number
  h: number
}

export interface Point {
  x: number
  y: number
}

// The infra pod renders as sub-boxes (kdns, then envoy wrapping its LDS,
// RBAC, and EDS tables), and each sub-box is a dot anchor, so a traced
// request lands on the exact component doing that step's work.
export interface InfraLayout {
  box: Rect
  kdns: Rect
  envoy: Rect
  lds: Rect
  rbac: Rect
  eds: Rect
}

export interface NodeLayout {
  card: Rect
  infra: InfraLayout
  slots: Record<string, Rect> // instance name → chip rect
}

export interface Layout {
  width: number
  height: number
  services: Record<string, Rect>
  nodes: Record<string, NodeLayout>
  pendingTray?: Rect
  pendingSlots: Record<string, Rect>
  // 'service:b' | 'kdns:node-1' | 'rbac:node-1' | 'instance:b-2' → dot anchor
  anchors: Record<string, Point>
}

const GAP = 16
const SERVICE_H = 112
const SERVICE_W = 200
const NODE_MIN_W = 340
const NODE_HEADER_H = 56
const CHIP_W = 148
const CHIP_H = 72
const CHIP_GAP = 10
const CARD_PAD = 14
const TRAY_H = 96

// infra pod internals. NodeCard renders these rects verbatim
const RECORD_H = 15 // one mono table line
const INFRA_HEAD_H = 20 // "infra pod — one shared netns · ip"
const SUB_TITLE_H = 16
const SUB_PAD = 9 // vertical padding + borders of one sub-box
const FOOT_H = 13 // small print under a table
const SUB_GAP = 5
const INFRA_PAD = 8

const kdnsH = (s: number) => SUB_TITLE_H + s * RECORD_H + FOOT_H + SUB_PAD
const ldsH = (s: number) => SUB_TITLE_H + s * RECORD_H + SUB_PAD
const RBAC_H = SUB_TITLE_H + 2 * 13 + SUB_PAD // always two summary lines
const edsH = (s: number) => SUB_TITLE_H + s * RECORD_H + FOOT_H + SUB_PAD
const envoyH = (s: number) => SUB_TITLE_H + ldsH(s) + SUB_GAP + RBAC_H + SUB_GAP + edsH(s) + SUB_PAD
const infraH = (s: number) => INFRA_PAD * 2 + INFRA_HEAD_H + kdnsH(s) + SUB_GAP + envoyH(s)

const center = (r: Rect): Point => ({ x: r.x + r.w / 2, y: r.y + r.h / 2 })

export function computeLayout(s: Snapshot, width: number): Layout {
  const services = sortedServices(s)
  const nodes = sortedNodes(s)
  const byNode = instancesByNode(s)
  const pending = pendingInstances(s)

  const layout: Layout = {
    width,
    height: 0,
    services: {},
    nodes: {},
    pendingSlots: {},
    anchors: {},
  }

  // service strip along the top. Policies get a band above it for their arcs
  let y = GAP + (Object.keys(s.policies).length > 0 ? 48 : 0)
  services.forEach((svc: Service, i) => {
    const perRow = Math.max(1, Math.floor((width - GAP) / (SERVICE_W + GAP)))
    const row = Math.floor(i / perRow)
    const col = i % perRow
    const rect = {
      x: GAP + col * (SERVICE_W + GAP),
      y: y + row * (SERVICE_H + GAP),
      w: SERVICE_W,
      h: SERVICE_H,
    }
    layout.services[svc.metadata.name] = rect
    layout.anchors[`service:${svc.metadata.name}`] = center(rect)
  })
  if (services.length > 0) {
    const rows = Math.ceil(services.length / Math.max(1, Math.floor((width - GAP) / (SERVICE_W + GAP))))
    y += rows * (SERVICE_H + GAP) + GAP
  }

  // node cards in a grid, where card height grows with its instance rows
  const columns = Math.max(1, Math.floor((width - GAP) / (NODE_MIN_W + GAP)))
  const cardW = Math.floor((width - GAP * (columns + 1)) / columns)
  const chipsPerRow = Math.max(1, Math.floor((cardW - CARD_PAD * 2) / (CHIP_W + CHIP_GAP)))
  const infraBoxH = infraH(services.length)

  const heights = nodes.map((n: NodeObj) => {
    const count = (byNode.get(n.metadata.name) ?? []).length
    const rows = Math.max(1, Math.ceil(count / chipsPerRow))
    return NODE_HEADER_H + infraBoxH + 10 + rows * (CHIP_H + CHIP_GAP) + CARD_PAD * 2
  })

  const colBottoms = Array.from({ length: columns }, () => y)
  nodes.forEach((node: NodeObj, i) => {
    // place into the shortest column so mixed-size cards pack tightly
    let col = 0
    for (let c = 1; c < columns; c++) if (colBottoms[c] < colBottoms[col]) col = c
    const x = GAP + col * (cardW + GAP)
    const cardY = colBottoms[col]
    const card = { x, y: cardY, w: cardW, h: heights[i] }
    colBottoms[col] = cardY + heights[i] + GAP

    const s = services.length
    const box = { x: x + CARD_PAD, y: cardY + NODE_HEADER_H, w: cardW - CARD_PAD * 2, h: infraBoxH }
    const ix = box.x + INFRA_PAD
    const iw = box.w - INFRA_PAD * 2
    const kdns = { x: ix, y: box.y + INFRA_PAD + INFRA_HEAD_H, w: iw, h: kdnsH(s) }
    const envoy = { x: ix, y: kdns.y + kdns.h + SUB_GAP, w: iw, h: envoyH(s) }
    const ex = envoy.x + 6
    const ew = iw - 12
    const lds = { x: ex, y: envoy.y + SUB_TITLE_H, w: ew, h: ldsH(s) }
    const rbac = { x: ex, y: lds.y + lds.h + SUB_GAP, w: ew, h: RBAC_H }
    const eds = { x: ex, y: rbac.y + rbac.h + SUB_GAP, w: ew, h: edsH(s) }
    const infra: InfraLayout = { box, kdns, envoy, lds, rbac, eds }
    const slots: Record<string, Rect> = {}
    const insts = byNode.get(node.metadata.name) ?? []
    insts.forEach((inst: Instance, j) => {
      const row = Math.floor(j / chipsPerRow)
      const colIdx = j % chipsPerRow
      const rect = {
        x: x + CARD_PAD + colIdx * (CHIP_W + CHIP_GAP),
        y: cardY + NODE_HEADER_H + infraBoxH + 10 + row * (CHIP_H + CHIP_GAP),
        w: CHIP_W,
        h: CHIP_H,
      }
      slots[inst.metadata.name] = rect
      layout.anchors[`instance:${inst.metadata.name}`] = center(rect)
    })

    layout.nodes[node.metadata.name] = { card, infra, slots }
    const n = node.metadata.name
    layout.anchors[`infra:${n}`] = center(box)
    layout.anchors[`kdns:${n}`] = center(kdns)
    layout.anchors[`lds:${n}`] = center(lds)
    layout.anchors[`rbac:${n}`] = center(rbac)
    layout.anchors[`eds:${n}`] = center(eds)
  })
  y = Math.max(...colBottoms, y)

  // unschedulable instances wait in a tray at the bottom, wrapping into rows
  if (pending.length > 0) {
    const perRow = Math.max(1, Math.floor((width - GAP * 2 - CARD_PAD * 2) / (CHIP_W + CHIP_GAP)))
    const rows = Math.ceil(pending.length / perRow)
    const trayH = TRAY_H + (rows - 1) * (CHIP_H + CHIP_GAP)
    const tray = { x: GAP, y, w: width - GAP * 2, h: trayH }
    layout.pendingTray = tray
    pending.forEach((inst, i) => {
      const rect = {
        x: tray.x + CARD_PAD + (i % perRow) * (CHIP_W + CHIP_GAP),
        y: tray.y + 20 + Math.floor(i / perRow) * (CHIP_H + CHIP_GAP),
        w: CHIP_W,
        h: CHIP_H,
      }
      layout.pendingSlots[inst.metadata.name] = rect
      layout.anchors[`instance:${inst.metadata.name}`] = center(rect)
    })
    y += trayH + GAP
  }

  layout.height = y + GAP
  return layout
}
