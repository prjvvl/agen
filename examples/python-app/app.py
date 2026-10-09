"""Order desk: an embedded Agen agent with a custom Python tool.

    python app.py            # scripted model, no API key needed
    python app.py --live     # real model via OpenRouter ($OPENROUTER_API_KEY)
"""

import sys

from agen import Agent, fake, openrouter, tool

ORDERS = {"A-1001": {"status": "shipped", "carrier": "UPS", "date": "2026-09-25"}}


@tool(read_only=True)
def lookup_order(order_id: str) -> dict:
    """Look up an order by id. Returns its status, carrier and ship date."""
    return ORDERS.get(order_id, {"error": f"no order {order_id}"})


def main() -> None:
    live = "--live" in sys.argv
    provider = openrouter() if live else fake(
        {"toolCalls": [{"name": "lookup_order", "arguments": {"order_id": "A-1001"}}]},
        {"expect": "shipped", "text": "Order A-1001 shipped on 2026-09-25 via UPS."},
    )
    agent = Agent(
        "order-desk",
        instructions="You answer order questions. Always use lookup_order, then answer in one sentence.",
        model="deepseek/deepseek-v4-flash",
        provider=provider,
        tools=[lookup_order],
        max_output_tokens=400,
    )
    with agent:
        result = agent.run("Where is my order A-1001?", on_delta=lambda s: print(s, end="", flush=True))
        print(f"\n\nstatus={result.status} tokens={result.usage.input_tokens}+{result.usage.output_tokens}")
        print("trace:")
        for span in agent.trace(result.trace_id):
            print(f"  {span['name']:<16} {span['endMs'] - span['startMs']:>5} ms  {span['status']}")
        if not result.ok:
            sys.exit(1)


if __name__ == "__main__":
    main()
