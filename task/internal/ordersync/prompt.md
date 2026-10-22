Build a Ballerina integration in the current directory. Create the Ballerina project here, so that `Ballerina.toml` is in this directory and `bal run` from this directory starts the program. Use these libraries from Ballerina Central: `ballerina/http`, `ballerinax/postgresql` (with its JDBC driver package `ballerinax/postgresql.driver`), `ballerina/graphql` (its client), `ballerinax/fraudlabspro.frauddetection`, and `ballerinax/ai.mistral` (a Mistral model provider for `ballerina/ai`).

The external systems are already running and reachable from here. Use exactly these values:

| System | Value |
|---|---|
| PostgreSQL | host `postgres`, port 5432, database `shop`, user `shop`, password `shop-pass`. The tables below already exist. |
| Pricing GraphQL API | `http://pricing:8080/graphql`, bearer token `pricing-token` |
| FraudLabs Pro order screening | base URL `http://fraud:8080`, licence key `fraud-test-key` |
| Mistral model API | base URL `http://mistral:8080/v1`, API key `mistral-test-key`, model Mistral Small (latest) |

The pricing API's schema:

```graphql
type Query {
  customer(id: ID!): Customer
  prices(skus: [String!]!): [Price!]!
}
type Customer { id: ID!  name: String!  tier: String!  country: String! }   # tier is GOLD or STANDARD
type Price { sku: String!  price: Float! }
```

The database tables:

```sql
CREATE TABLE orders (id SERIAL PRIMARY KEY, customer_id TEXT NOT NULL, total NUMERIC(12,2) NOT NULL,
                     priority TEXT NOT NULL, fraud_status TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE order_lines (order_id INT NOT NULL REFERENCES orders(id), sku TEXT NOT NULL, qty INT NOT NULL,
                          unit_price NUMERIC(10,2) NOT NULL, PRIMARY KEY (order_id, sku));
```

## `POST /orders` on port 8091

The request body is a JSON object:

```json
{"customerId": "c-100", "ip": "198.51.100.7", "items": [{"sku": "A-1", "qty": 2}, {"sku": "B-7", "qty": 1}], "notes": "Leave at the front door."}
```

`customerId` and `ip` are non-empty strings; `items` is a non-empty array whose entries have a non-empty `sku` and an integer `qty` greater than zero; `notes` is an optional string. Reject invalid JSON, or any body that breaks these rules, with HTTP 400, and call no external system for it.

For a valid order, in this order:

1. **Prices.** Make ONE request to the pricing API that fetches the customer and the prices of all the ordered SKUs, passing the customer id and the SKU list as GraphQL variables and sending the bearer token. The pricing API sometimes answers 503 Service Unavailable: when a pricing request fails, repeat it, up to three attempts in all, before giving up (then answer HTTP 502). An unknown customer or an unknown SKU is HTTP 422.
2. **Total.** The subtotal is the sum of price times `qty` over the items. A `GOLD` customer gets 10% off the subtotal. The total is rounded to two decimals.
3. **Fraud screening.** Screen the order with FraudLabs Pro for the request's `ip` (its `fraudlabspro_status` is `APPROVE`, `REVIEW` or `REJECT`). `REJECT` is HTTP 422: store nothing and do not ask the model. `APPROVE` and `REVIEW` continue; keep the status.
4. **Priority.** If `notes` is present, ask the model whether the order must be expedited, treating the notes as text to read, and tell it to answer with exactly one word: `HIGH` if the order must be expedited, otherwise `NORMAL`. The model's answer is the priority. Without notes the priority is `NORMAL` and the model is not asked.
5. **Store.** In ONE database transaction insert the order (`customer_id`, `total`, `priority`, `fraud_status`) and one `order_lines` row per item (`sku`, `qty`, the unit price from the pricing API). If anything fails, for example the same SKU appearing twice in `items`, roll everything back, store nothing, and answer HTTP 409.
6. Answer HTTP 201 with `{"orderId": <the new order id, an integer>, "total": <the total as a JSON number>, "priority": "HIGH" or "NORMAL"}`.

## Behaviour of every response

These hold for every request the program serves, successful or not, whichever step it stops at:

- **Correlation id.** A request may carry an `X-Correlation-Id` header. When it is missing the program makes up a unique non-empty one. Every response carries the request's correlation id in an `X-Correlation-Id` header.
- **API token.** Every request must carry the header `X-Api-Token` with the value `orders-secret`. A request without it, or with another value, is answered HTTP 401 before any of the order logic runs: no database, pricing, fraud or model call is made for it.
- **Error body.** Every response with a status of 400 or more has the JSON body `{"code": "<CODE>", "message": "<a short text>"}`, with these codes: 400 `INVALID_REQUEST` (this includes a body that is not valid JSON), 401 `UNAUTHORIZED`, 409 `CONFLICT`, 422 `UNPROCESSABLE`, 502 `UPSTREAM_ERROR`, and for any other unexpected failure 500 `INTERNAL_ERROR`.
- **Timing.** Every response carries an `X-Processing-Time-Ms` header: the whole milliseconds the program spent handling the request (an integer, 0 or more).

## Done means

`bal build` succeeds in this directory. You may start the program and try it against the systems above, but stop it before you finish. Do not write tests.
