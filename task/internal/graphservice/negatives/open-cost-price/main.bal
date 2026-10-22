import ballerina/graphql;
import ballerina/http;
import ballerina/ai;
import ballerina/log;
import ballerina/mqtt;
import ballerina/sql;
import ballerinax/ai.ollama;
import ballerinax/mailchimp.'transactional as mandrill;
import ballerinax/mysql;
import ballerinax/mysql.driver as _;

const ROLE_KEY = "role";
const ANONYMOUS = "ANONYMOUS";
const STAFF = "STAFF";
const ADMIN = "ADMIN";
const STAFF_TOKEN = "staff-token";
const ADMIN_TOKEN = "admin-token";
const BEARER_PREFIX = "Bearer ";
const FORBIDDEN = "forbidden";
const OUT_OF_STOCK = "out of stock";
const PRODUCT_TOPIC = "products/created";
const ORDER_TOPIC = "orders/placed";
const MANDRILL_KEY = "mandrill-test-key";
const SENDER = "shop@example.com";
const CENTS = 100.0;

final mysql:Client db = check new (host = "mysql", user = "shop", password = "shop-pass", database = "shop", port = 3306);
final mqtt:Client broker = check new ("tcp://mosquitto:1883", "graph-service");
final ollama:ModelProvider model = check new ("llama3.2", "http://ollama:8080");
final mandrill:Client mailer = check new (serviceUrl = "http://mandrill:8080/api/1.0");

type ProductRow record {|
    int id;
    string name;
    decimal price;
    decimal cost_price;
    int stock;
    string? description;
|};

type OrderRow record {|
    int id;
    int product_id;
    int quantity;
    string customer_email;
    decimal total;
|};

isolated function initContext(http:RequestContext requestContext, http:Request request) returns graphql:Context|error {
    graphql:Context context = new;
    string role = ANONYMOUS;
    string|http:HeaderNotFoundError header = request.getHeader(http:AUTH_HEADER);
    if header is string && header.startsWith(BEARER_PREFIX) {
        string token = header.substring(BEARER_PREFIX.length());
        if token == ADMIN_TOKEN {
            role = ADMIN;
        } else if token == STAFF_TOKEN {
            role = STAFF;
        }
    }
    context.set(ROLE_KEY, role);
    return context;
}

readonly service class RoleGuard {
    *graphql:Interceptor;
    private final string required;

    isolated function init(string required) {
        self.required = required;
    }

    isolated remote function execute(graphql:Context context, graphql:Field 'field) returns anydata|error {
        string role = check context.get(ROLE_KEY).ensureType();
        boolean allowed = role == ADMIN || (role == STAFF && self.required == STAFF);
        if !allowed {
            return error(FORBIDDEN);
        }
        return context.resolve('field);
    }
}

distinct service class Product {
    private final readonly & ProductRow row;

    isolated function init(ProductRow row) {
        self.row = row.cloneReadOnly();
    }

    isolated resource function get id() returns @graphql:ID int => self.row.id;

    isolated resource function get name() returns string => self.row.name;

    isolated resource function get price() returns float => <float>self.row.price;

    isolated resource function get description() returns string? => self.row.description;

    @graphql:ResourceConfig {interceptors: new RoleGuard(STAFF)}
    isolated resource function get stock() returns int? => self.row.stock;

    isolated resource function get costPrice() returns float? => <float>self.row.cost_price;
}

distinct service class Order {
    private final readonly & OrderRow row;

    isolated function init(OrderRow row) {
        self.row = row.cloneReadOnly();
    }

    isolated resource function get id() returns @graphql:ID int => self.row.id;

    isolated resource function get product() returns Product|error {
        ProductRow product = check db->queryRow(`SELECT * FROM products WHERE id = ${self.row.product_id}`);
        return new Product(product);
    }

    isolated resource function get quantity() returns int => self.row.quantity;

    isolated resource function get total() returns float => <float>self.row.total;

    @graphql:ResourceConfig {interceptors: new RoleGuard(STAFF)}
    isolated resource function get customerEmail() returns string? => self.row.customer_email;
}

@graphql:ServiceConfig {contextInit: initContext}
service /graphql on new graphql:Listener(9090) {
    isolated resource function get products() returns Product[]|error {
        stream<ProductRow, sql:Error?> rows = db->query(`SELECT * FROM products ORDER BY id`);
        return from ProductRow row in rows
            select new Product(row);
    }

    isolated resource function get product(@graphql:ID int id) returns Product|error? {
        ProductRow|sql:Error row = db->queryRow(`SELECT * FROM products WHERE id = ${id}`);
        if row is sql:NoRowsError {
            return ();
        }
        return new Product(check row);
    }

    @graphql:ResourceConfig {interceptors: new RoleGuard(STAFF)}
    isolated resource function get orders() returns Order[]?|error {
        stream<OrderRow, sql:Error?> rows = db->query(`SELECT id, product_id, quantity, customer_email, total FROM orders ORDER BY id`);
        return from OrderRow row in rows
            select new Order(row);
    }

    @graphql:ResourceConfig {interceptors: new RoleGuard(ADMIN)}
    isolated remote function createProduct(string name, float price, float costPrice, int stock)
            returns Product|error? {
        ai:ChatAssistantMessage reply = check model->chat({
            role: ai:USER,
            content: string `Write one sentence for a shop catalog describing the product "${name}".`
        });
        string description = (reply.content ?: "").trim();
        sql:ExecutionResult result = check db->execute(`INSERT INTO products (name, price, cost_price, stock, description)
            VALUES (${name}, ${price}, ${costPrice}, ${stock}, ${description})`);
        int id = check result.lastInsertId.ensureType();
        _ = check broker->publish(PRODUCT_TOPIC, {payload: string `{"id": ${id}, "name": ${name.toJsonString()}}`.toBytes()});
        ProductRow created = check db->queryRow(`SELECT * FROM products WHERE id = ${id}`);
        return new Product(created);
    }

    isolated remote function placeOrder(@graphql:ID int productId, int quantity, string customerEmail)
            returns Order|error? {
        OrderRow placed = check store(productId, quantity, customerEmail);
        ProductRow product = check db->queryRow(`SELECT * FROM products WHERE id = ${productId}`);
        mandrill:InlineResponse20028[]|error sent = mailer->/messages/send.post({
            'key: MANDRILL_KEY,
            message: {
                fromEmail: SENDER,
                subject: string `Order ${placed.id} confirmed`,
                text: string `Thank you for ordering ${quantity} x ${product.name}. Total: ${placed.total}.`,
                to: [{email: customerEmail, 'type: "to"}]
            }
        });
        if sent is error {
            log:printError("confirmation email failed", sent);
        }
        _ = check broker->publish(ORDER_TOPIC, {payload: string `{"orderId": ${placed.id}, "productId": ${productId}, "quantity": ${quantity}}`.toBytes()});
        return new Order(placed);
    }
}

isolated function store(int productId, int quantity, string customerEmail) returns OrderRow|error {
    transaction {
        ProductRow product = check db->queryRow(`SELECT * FROM products WHERE id = ${productId} FOR UPDATE`);
        if product.stock < quantity {
            fail error(OUT_OF_STOCK);
        }
        _ = check db->execute(`UPDATE products SET stock = stock - ${quantity} WHERE id = ${productId}`);
        decimal total = product.price * <decimal>quantity;
        sql:ExecutionResult result = check db->execute(`INSERT INTO orders (product_id, quantity, customer_email, total)
            VALUES (${productId}, ${quantity}, ${customerEmail}, ${total})`);
        check commit;
        int id = check result.lastInsertId.ensureType();
        return {id, product_id: productId, quantity, customer_email: customerEmail, total};
    } on fail error failure {
        return failure;
    }
}
