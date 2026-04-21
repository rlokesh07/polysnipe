"""Posts a small test order to a currently active up/down market via py_clob_client."""
import json, os, sys
from py_clob_client.client import ClobClient, ApiCreds
from py_clob_client.clob_types import OrderArgs, OrderType

for var in ["POLYMARKET_WALLET_KEY", "POLYMARKET_FUNDER_ADDRESS",
            "POLYMARKET_API_KEY", "POLYMARKET_API_SECRET", "POLYMARKET_PASSPHRASE"]:
    if not os.environ.get(var):
        print(f"ERROR: {var} not set"); sys.exit(1)

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

# Find a live up/down market
import requests
resp = requests.get("https://gamma-api.polymarket.com/markets?active=true&closed=false&limit=20&tag_slug=up-or-down")
markets = resp.json()
market = next((m for m in markets if m.get("active") and not m.get("closed")), None)
if not market:
    print("No active up/down market found"); sys.exit(1)

import ast
token_ids = json.loads(market["clobTokenIds"])
token_id = token_ids[0]
print(f"Market: {market['question']}")
print(f"Token:  {token_id}")

neg_risk = client.get_neg_risk(token_id)
fee_rate = client.get_fee_rate_bps(token_id)
print(f"neg_risk: {neg_risk}, fee_rate: {fee_rate}")

# Get current mid price
book = requests.get(f"https://clob.polymarket.com/book?token_id={token_id}").json()
bids = book.get("bids", [])
asks = book.get("asks", [])
if bids and asks:
    mid = (float(bids[0]["price"]) + float(asks[0]["price"])) / 2
else:
    mid = 0.5
print(f"Mid price: {mid:.3f}")

order = client.create_order(OrderArgs(token_id=token_id, price=round(mid, 2), size=1.0, side="BUY"))
print("\nPosting order...")
result = client.post_order(order, OrderType.GTC)
print("Result:", result)
