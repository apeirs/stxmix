// An unmodified stacks.js client (@stacks/network + @stacks/transactions 7.x)
// pointed at stxmix serve. It knows nothing about Nym: only the base URL differs.
//
//   stxmix serve &
//   node stacksjs-client.mjs [http://127.0.0.1:3999]
import { createNetwork } from '@stacks/network';
import {
  Cl, cvToString, fetchAbi, fetchCallReadOnlyFunction, fetchNonce, fetchFeeEstimateTransfer, broadcastTransaction,
} from '@stacks/transactions';

const base = process.argv[2] || 'http://127.0.0.1:3999';
const mainnet = createNetwork({ network: 'mainnet', client: { baseUrl: base } });
const testnet = createNetwork({ network: 'testnet', client: { baseUrl: base + '/testnet' } });
const sbtc = { contractAddress: 'SM3VDXK3WZZSA84XXFKAFAF15NNZX32CTSG82JFQ4', contractName: 'sbtc-token' };

async function step(name, fn) {
  const t0 = performance.now();
  try {
    const out = await fn();
    console.log(`${name}: ${out}  (${((performance.now() - t0) / 1000).toFixed(1)} s)`);
  } catch (e) {
    console.log(`${name}: ERROR ${e.message}${e.cause ? ' / ' + e.cause.message : ''}  (${((performance.now() - t0) / 1000).toFixed(1)} s)`);
  }
}

console.log(`stacks.js network baseUrl = ${mainnet.client.baseUrl} (mainnet), ${testnet.client.baseUrl} (testnet)`);

await step('fetch(/v2/info) via network.client', async () => {
  const r = await (mainnet.client.fetch ?? fetch)(`${mainnet.client.baseUrl}/v2/info`);
  const j = await r.json();
  return `network_id=${j.network_id} stacks_tip_height=${j.stacks_tip_height}`;
});

await step('fetchCallReadOnlyFunction sbtc-token.get-total-supply', async () => {
  const cv = await fetchCallReadOnlyFunction({ ...sbtc, functionName: 'get-total-supply', functionArgs: [], senderAddress: sbtc.contractAddress, network: mainnet });
  return cvToString(cv);
});

await step('fetchCallReadOnlyFunction sbtc-token.get-balance', async () => {
  const cv = await fetchCallReadOnlyFunction({ ...sbtc, functionName: 'get-balance', functionArgs: [Cl.principal(sbtc.contractAddress)], senderAddress: sbtc.contractAddress, network: mainnet });
  return cvToString(cv);
});

await step('fetchAbi sbtc-token', async () => {
  const abi = await fetchAbi({ ...sbtc, network: mainnet });
  return `${abi.functions.filter((f) => f.access === 'read_only').length} read-only functions, e.g. ${abi.functions.filter((f) => f.access === 'read_only').slice(0, 3).map((f) => f.name).join(', ')}`;
});

await step('fetchNonce SP000000000000000000002Q6VF78', async () => String(await fetchNonce({ address: 'SP000000000000000000002Q6VF78', network: mainnet })));

// stacks.js 7.7.0 ignores `network` when fetchFeeEstimateTransfer gets a byte
// count instead of a transaction, and falls back to https://api.mainnet.hiro.so
// over the clearnet. Passing client: { baseUrl } as well keeps it on the gateway.
await step('fetchFeeEstimateTransfer (180 bytes, µSTX)', async () => String(await fetchFeeEstimateTransfer({ transaction: 180, network: mainnet, client: { baseUrl: base } })));

await step('testnet fetchNonce ST000000000000000000002AMW42H', async () => String(await fetchNonce({ address: 'ST000000000000000000002AMW42H', network: testnet })));

// stacks.js' own broadcast code path, fed junk bytes instead of a signed
// transaction: the node must reject it. No real transaction is ever sent.
await step('broadcastTransaction(junk bytes)', async () => {
  const junk = { serialize: () => '0000000001deadbeefdeadbeefdeadbeef00' };
  const r = await broadcastTransaction({ transaction: junk, network: mainnet });
  return 'answer: ' + JSON.stringify(r);
});
