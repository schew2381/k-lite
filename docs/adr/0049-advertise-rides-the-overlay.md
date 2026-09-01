# The advertise address rides the overlay when one exists

The first hotspot laptop join exposed an asymmetry the same-Wi-Fi test never could. The playground's open mode had local agents advertise the Mac's Wi-Fi address, so cross-machine traffic *into* the Mac's nodes dialed an address only the local network can route. The laptop advertised its tailnet address and was reachable, while every call it sent toward a Mac-hosted instance timed out at the ingress hop. The failure signature was distinctive: policy-denied pairs failed correctly, local picks succeeded, and remote picks failed, flipping call by call with EDS's endpoint choice. The decision: when the host carries a tailnet address (an interface IP in 100.64.0.0/10, the same classification `node add` and the facade use), dev-up and demo advertise that address instead of the Wi-Fi one. ADR 0043 made the overlay the internet answer, and an advertise address is a promise to every future joiner, so the address with the widest reach wins. Applied live by restarting the four local agents with the tailnet advertise, with no reseed and no interruption to the joined laptop.

## Considered Options

1. **Keep the Wi-Fi advertise.** Right for a LAN-only laptop without the overlay, and wrong for every machine beyond the LAN. It also fails quietly: joins succeed, local traffic flows, and only remote endpoint picks die.
2. **Advertise the tailnet address when present (chosen).** Every joiner runs tailscale anyway, because join.sh and the setup scripts install it, and a tailnet-joined machine on the same Wi-Fi still reaches the tailnet address. The Wi-Fi address stays the fallback when no overlay exists.
3. **Advertise several addresses per node and let the dialer pick.** The honest general answer and a protocol change: IngressAllocations, EDS shapes, and the agent's report all carry one address today. Recorded as the future fix if mixed fleets (some machines on the overlay, some not) ever matter.

## Consequences

- With the overlay present, cross-node traffic between local nodes also rides the tailnet address. Tailscale hairpins local peers efficiently, and the demo path stops depending on which network a caller sits on.
- A LAN laptop that skipped tailscale can no longer reach local instances cross-machine. That trade is deliberate: the setup path installs tailscale everywhere, and ADR 0043 already picked the overlay as the one internet story.
- The serving certificate already carries both addresses in its SANs, so nothing about TLS moved.
- Existing donors and Envoys pick the change up through ordinary re-registration and xDS pushes. A live cluster heals with an agent restart per local node.
