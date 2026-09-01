// Ipam hands out deterministic addresses inside klite0 (10.44.0.0/16), carved
// by role the way the real IPAM is: infra at 10.44.0.1x, VIPs in 10.44.64.0/18,
// instances from 10.44.128.0/17. The address tells you what a thing is.

export class Ipam {
  private nextInstance = 0
  private nextVip = 0
  private nodeIndex = new Map<string, number>()

  instanceIp(): string {
    const n = this.nextInstance++
    return `10.44.${128 + Math.floor(n / 254)}.${(n % 254) + 2}`
  }

  vip(): string {
    const n = this.nextVip++
    return `10.44.${64 + Math.floor(n / 254)}.${(n % 254) + 2}`
  }

  // indexes start at 1, matching the server's freeNodeIndex
  indexOf(node: string): number {
    let idx = this.nodeIndex.get(node)
    if (idx === undefined) {
      idx = this.nodeIndex.size + 1
      this.nodeIndex.set(node, idx)
    }
    return idx
  }

  infraIp(node: string): string {
    return `10.44.0.${10 + this.indexOf(node)}`
  }

  // Every node acts as its own machine on the open internet (TEST-NET-2
  // addresses, unmistakably not the cluster bridge).
  advertiseAddress(node: string): string {
    return `198.51.100.${this.indexOf(node)}`
  }

  // Ingress ports mirror NetBootstrap's base+range scheme: each node owns a
  // window and hands slots to its local endpoints. The slot rule is the
  // sim's own until M9's endpoints engine lands.
  private ingressSlots = new Map<string, number>()

  ingressPort(node: string): number {
    const used = this.ingressSlots.get(node) ?? 0
    this.ingressSlots.set(node, used + 1)
    return 30000 + (this.indexOf(node) - 1) * 512 + used
  }
}
