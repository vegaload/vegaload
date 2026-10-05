// smoke.vl.js -- the simplest possible VegaLoad scenario file: an
// example of "a test is a real file" (see AGENTS.md), not a load test.
// A scripted scenario can make real HTTP/WebSocket calls via the
// http/ws globals (see http-ws-chain.vl.js for that, and AGENTS.md),
// but this one deliberately doesn't -- it's here to exercise VegaLoad's
// VU pool and executor shapes against plain JS, which protocol-direct
// mode (-target/-protocol) can't do since it has no scripting of its
// own. See ../README.md for both.
//
// Run it (flags before the scenario file -- vegaload's flag parser
// stops at the first non-flag argument, see "vegaload run -h"):
//   vegaload run -vus 5 -duration 5s smoke.vl.js
export default function () {
  console.log("iteration");
}
