Build a Ballerina integration in the current directory. Create the Ballerina project here, so that `Ballerina.toml` is in this directory and `bal run` from this directory starts the program. Use these libraries from Ballerina Central: `ballerina/graphql`, `ballerina/http`, `ballerinax/mysql` (with its JDBC driver package `ballerinax/mysql.driver`), `ballerina/mqtt`, `ballerinax/mailchimp.transactional` (Mandrill, for email), and `ballerinax/ai.ollama` (an Ollama model provider for `ballerina/ai`).

The program is a GraphQL service for a small shop. It serves GraphQL at `http://<host>:9090/graphql` (port 9090, path `/graphql`) and runs until it is stopped.

The external systems are already running and reachable from here. Use exactly these values:

| System | Value |
|---|---|
| MySQL | host `mysql`, port 3306, database `shop`, user `shop`, password `shop-pass`. The tables below already exist; `products` holds three products. |
| MQTT broker | `tcp://mosquitto:1883`, no authentication |
| Mandrill (transactional email) | base URL `http://mandrill:8080/api/1.0`, API key `mandrill-test-key` |
| Ollama | base URL `http://ollama:8080`, model `llama3.2` |

```sql
CREATE TABLE products (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100) NOT NULL, price DECIMAL(10,2) NOT NULL,
                       cost_price DECIMAL(10,2) NOT NULL, stock INT NOT NULL, description TEXT);
CREATE TABLE orders (id INT AUTO_INCREMENT PRIMARY KEY, product_id INT NOT NULL, quantity INT NOT NULL,
                     customer_email VARCHAR(200) NOT NULL, total DECIMAL(12,2) NOT NULL,
                     created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP);
```

## The GraphQL schema

```graphql
type Query {
  products: [Product!]!
  product(id: ID!): Product
  orders: [Order!]
}
type Mutation {
  createProduct(name: String!, price: Float!, costPrice: Float!, stock: Int!): Product
  placeOrder(productId: ID!, quantity: Int!, customerEmail: String!): Order
}
type Product { id: ID!  name: String!  price: Float!  description: String  stock: Int  costPrice: Float }
type Order { id: ID!  product: Product!  quantity: Int!  total: Float!  customerEmail: String }
```

`products` and `product` read the `products` table (`product` is `null` for an unknown id); `orders` reads the `orders` table. `price`, `total` and `costPrice` are the table's decimals as floats.

## Who may see and do what

A request may carry the HTTP header `Authorization: Bearer <token>`. The token `staff-token` makes the caller a **STAFF** member and `admin-token` makes the caller an **ADMIN**. Any other request (no header, another scheme, an unknown token) is **anonymous**. An ADMIN may do everything a STAFF member may.

| What | Anonymous | STAFF | ADMIN |
|---|---|---|---|
| `Product` fields `id`, `name`, `price`, `description`; the queries `products` and `product`; the mutation `placeOrder`; `Order` fields `id`, `product`, `quantity`, `total` | yes | yes | yes |
| `Product.stock`; `Order.customerEmail`; the query `orders` | no | yes | yes |
| `Product.costPrice`; the mutation `createProduct` | no | no | yes |

When the caller may not have a field, that field is `null` in the answer and the response's `errors` list has an entry whose `message` is exactly `forbidden` and whose `path` ends with that field's name; every other field the query asked for is answered as usual. A mutation the caller may not run does nothing at all: nothing is written to the database, the model is not asked and no message is sent.

## What the mutations do

**`createProduct`** inserts a row in `products`. Its `description` is written by the model: ask the Ollama model for a one-sentence catalog description of the product (the prompt names the product) and store the model's text in `products.description`. Then publish an MQTT message on the topic `products/created` with QoS 1 and a JSON payload `{"id": <the new id>, "name": "<the name>"}`. It returns the new product.

**`placeOrder`** orders `quantity` units of the product, in ONE database transaction: if the product's stock is smaller than `quantity` the order fails with a GraphQL error (the field is `null`) and nothing changes; otherwise reduce the product's stock by `quantity` and insert an `orders` row whose `total` is the product's price times `quantity`. After the order is stored, send a confirmation email through Mandrill from `shop@example.com` to `customerEmail` with the subject `Order <id> confirmed` and a text that names the product, and publish an MQTT message on the topic `orders/placed` with QoS 1 and the JSON payload `{"orderId": <id>, "productId": <product id>, "quantity": <quantity>}`. A failed order sends no email and no message. If the email cannot be sent, the order still stands. It returns the new order.

## Done means

`bal build` succeeds in this directory. You may start the program and try it against the systems above, but stop it before you finish. Do not write tests.
