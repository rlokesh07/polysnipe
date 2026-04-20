import json
import os
import sys

try:
    from py_clob_client.client import ClobClient, ApiCreds
    from py_clob_client.clob_types import BalanceAllowanceParams, AssetType
except ImportError:
    print("ERROR: run 'pip install py-clob-client' first")
    sys.exit(1)

for var in ["POLYMARKET_WALLET_KEY", "POLYMARKET_API_KEY", "POLYMARKET_API_SECRET", "POLYMARKET_PASSPHRASE"]:
    if not os.environ.get(var):
        print(f"ERROR: {var} not set")
        sys.exit(1)

client = ClobClient(
    "https://clob.polymarket.com",
    chain_id=137,
    key=os.environ["POLYMARKET_WALLET_KEY"],
    creds=ApiCreds(
        api_key=os.environ["POLYMARKET_API_KEY"],
        api_secret=os.environ["POLYMARKET_API_SECRET"],
        api_passphrase=os.environ["POLYMARKET_PASSPHRASE"],
    ),
    signature_type=0,
)

print("=== GET /balance-allowance?asset_type=COLLATERAL ===")
print(json.dumps(client.get_balance_allowance(BalanceAllowanceParams(asset_type=AssetType.COLLATERAL)), indent=2))

print("\n=== GET /data/orders?status=OPEN ===")
print(json.dumps(client.get_orders(), indent=2))

print("\n=== sample markets ===")
markets = client.get_markets()
data = markets.get("data", []) if isinstance(markets, dict) else (markets if isinstance(markets, list) else [])
for m in data[:2]:
    print(json.dumps(m, indent=2))
    print("---")
