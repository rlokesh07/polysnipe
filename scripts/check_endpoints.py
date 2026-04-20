import json
import os
import sys

# Patch requests to log URLs before importing py_clob_client
import requests
original_request = requests.Session.request
def patched_request(self, method, url, **kwargs):
    print(f"  [HTTP] {method} {url}", file=sys.stderr)
    return original_request(self, method, url, **kwargs)
requests.Session.request = patched_request

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

print("=== balance ===")
result = client.get_balance_allowance(BalanceAllowanceParams(asset_type=AssetType.COLLATERAL))
print(json.dumps(result, indent=2))

print("\n=== open orders ===")
result = client.get_orders()
print(json.dumps(result, indent=2))
