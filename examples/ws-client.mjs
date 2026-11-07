// Hiro's JSON-RPC websocket (/extended/v1/ws) through stxmix serve, using
// Node's built-in WebSocket: subscribe to new blocks, print two, then close.
const url = process.argv[2] || 'ws://127.0.0.1:3999/extended/v1/ws';
const t0 = performance.now();
const s = () => ((performance.now() - t0) / 1000).toFixed(1) + ' s';
const ws = new WebSocket(url);
let blocks = 0;
const stop = setTimeout(() => { console.log('no block within 120 s'); ws.close(); process.exit(1); }, 120000);
ws.onopen = () => {
  console.log(`open ${url} after ${s()}`);
  ws.send(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'subscribe', params: { event: 'block' } }));
};
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id === 1) { console.log(`subscribed after ${s()}: ${JSON.stringify(m.result ?? m.error)}`); return; }
  if (m.method === 'block') {
    console.log(`block ${m.params.height} ${m.params.block_time_iso ?? ''} txs=${m.params.txs?.length ?? '?'} after ${s()}`);
    if (++blocks === 2) { clearTimeout(stop); ws.close(); }
  }
};
ws.onerror = (e) => { console.log('error', e.message ?? e); };
ws.onclose = (e) => { console.log(`closed (${e.code}) after ${s()}`); };
