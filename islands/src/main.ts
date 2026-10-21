// The island bundle: registers <fleet-agent> (docs/adr/0068). Hosts load it
// with <script type="module" src="https://fleet.bnei.dev/islands/fleet-agent.js">.
import "./FleetAgent.svelte";

export { setTransport } from "./api";
