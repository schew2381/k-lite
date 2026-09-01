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

  infraIp(node: string): string {
    let idx = this.nodeIndex.get(node)
    if (idx === undefined) {
      idx = this.nodeIndex.size
      this.nodeIndex.set(node, idx)
    }
    return `10.44.0.${10 + idx}`
  }
}
