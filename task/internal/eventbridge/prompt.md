Build a Ballerina integration in the current directory. Create the Ballerina project here, so that `Ballerina.toml` is in this directory and `bal run` from this directory starts the program. Use these libraries from Ballerina Central: `ballerinax/rabbitmq`, `ballerinax/nats` (with its JetStream support), and `ballerinax/pagerduty`.

The program is a bridge: it reads order events from the RabbitMQ queue `orders.incoming`, passes the good ones on to a NATS JetStream stream, and sends the bad ones to a dead-letter queue. It runs until it is stopped and has no HTTP endpoint.

The external systems are already running and reachable from here. Use exactly these values:

| System | Value |
|---|---|
| RabbitMQ | host `rabbitmq`, port 5672, user `guest`, password `guest`, default virtual host. The direct exchange `orders.dlx` and the queue `orders.dead` (bound to that exchange with the routing key `orders.dead`) already exist. The queue `orders.incoming` does not exist yet. |
| NATS | `nats://nats:4222`, JetStream enabled, no authentication. No stream exists yet. |
| PagerDuty (REST API) | base URL `http://pagerduty:8080`, API key `pd-test-key`. Incidents are created for the service with id `PSVC001`, on behalf of the user `ops@example.com`. |

## The order events

Every message on `orders.incoming` should be a JSON object:

```json
{"orderId": "o-1001", "customerId": "acme", "amount": 125.5, "currency": "EUR"}
```

An event is **good** when it is valid JSON, `orderId` and `customerId` are non-empty strings, `amount` is a number greater than zero and `currency` is exactly three upper-case letters. Other fields may be present and are allowed. Anything else is a **poison message**: a body that is not valid JSON, a missing or empty field, a wrong type, an `amount` of zero or less, a `currency` that is not three upper-case letters.

## What the program does

1. **Queue.** The program declares the durable queue `orders.incoming` and consumes it. The queue is set up so that a message which is rejected without being put back is dead-lettered to the exchange `orders.dlx` with the routing key `orders.dead`, which lands it in `orders.dead`.
2. **Stream.** When it starts, the program creates the JetStream stream `ORDERS`. The stream collects the subjects `orders.created.*`, keeps its events on disk (not in memory), keeps each event for 24 hours and holds at most 100000 events.
3. **Good events.** Every good event is published to the stream, with the message body exactly as it was received, on the subject `orders.created.<currency in lower case>` (a `EUR` event goes to `orders.created.eur`). Events must reach the stream in the order they were read. Only after the event is in the stream is the RabbitMQ message acknowledged, so that a message is never lost between the two systems.
4. **Poison messages.** A poison message is rejected, which is the signal the broker uses to move it to `orders.dead`. It is not put back on the queue, so it is never delivered again, it is not published to the stream, and the consumer carries on with the next message.
5. **Alert.** When the program has rejected its third poison message since it started, it creates one PagerDuty incident for the service `PSVC001`. The incident has the title `Order bridge: 3 poison messages dead-lettered` (the number is the count at that moment, 3), high urgency, and is created on behalf of `ops@example.com`. The fourth and later poison messages do not create further incidents. A failure to reach PagerDuty must not stop the bridge.

## Done means

`bal build` succeeds in this directory. You may start the program and try it against the systems above (the queues and the stream are cleared before the program is graded), but stop it before you finish. Do not write tests.
