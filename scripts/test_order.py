"""
Builds and prints a signed order using the official py_clob_client.
Run with the same token ID the Go bot is trying to trade to compare payloads.
"""
import json
import os
import sys

try:
    from py_clob_client.client import ClobClient, ApiCreds
    from py_clob_client.clob_types import OrderArgs, OrderType
    from py_clob_client.utilities import order_to_json
except ImportError:
    print("ERROR: run 'pip install py-clob-client' first")
    sys.exit(1)

for var in ["POLYMARKET_WALLET_KEY", "POLYMARKET_FUNDER_ADDRESS",
            "POLYMARKET_API_KEY", "POLYMARKET_API_SECRET", "POLYMARKET_PASSPHRASE"]:
    if not os.environ.get(var):
        print(f"ERROR: {var} not set")
        sys.exit(1)

# Token ID from a recent failing Go order — update this to match current logs
TOKEN_ID = sys.argv[1] if len(sys.argv) > 1 else "74415791238722271346945832713657612588229741048332182254958960302120464603333"
PRICE   = float(sys.argv[2]) if len(sys.argv) > 2 else 0.44
SIZE    = float(sys.argv[3]) if len(sys.argv) > 3 else 1.0

client = ClobClient(
    "https://clob.polymarket.com",
    chain_id=137,
    key=os.environ["POLYMARKET_WALLET_KEY"],
    creds=ApiCreds(
        api_key=os.environ["POLYMARKET_API_KEY"],
        api_secret=os.environ["POLYMARKET_API_SECRET"],
        api_passphrase=os.environ["POLYMARKET_PASSPHRASE"],
    ),
    signature_type=2,
    funder=os.environ["POLYMARKET_FUNDER_ADDRESS"],
)

neg_risk = client.get_neg_risk(TOKEN_ID)
print(f"neg_risk for token {TOKEN_ID}: {neg_risk}")

order = client.create_order(OrderArgs(
    token_id=TOKEN_ID,
    price=PRICE,
    size=SIZE,
    side="BUY",
))

body = order_to_json(order, client.creds.api_key, OrderType.GTC, False)
print("\nPayload that py_clob_client would send:")
print(json.dumps(body, indent=2))
