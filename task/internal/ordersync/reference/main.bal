import ballerina/ai;
import ballerina/graphql;
import ballerina/http;
import ballerina/lang.value;
import ballerina/log;
import ballerina/sql;
import ballerina/time;
import ballerina/uuid;
import ballerinax/ai.mistral;
import ballerinax/fraudlabspro.frauddetection as fraud;
import ballerinax/postgresql;
import ballerinax/postgresql.driver as _;

const PRICING_QUERY = string `query ($id: ID!, $skus: [String!]!) {
    customer(id: $id) { name tier }
    prices(skus: $skus) { sku price }
}`;
const GOLD = "GOLD";
const HIGH = "HIGH";
const NORMAL = "NORMAL";
const REJECT = "REJECT";
const GOLD_DISCOUNT = 0.9d;

type OrderItem record {|
    string sku;
    int qty;
|};

type OrderRequest record {|
    string customerId;
    string ip;
    OrderItem[] items;
    string notes?;
|};

type Customer record {|
    string name;
    string tier;
|};

type Price record {|
    string sku;
    float price;
|};

type PricingResponse record {|
    record {|
        Customer? customer;
        Price[] prices;
    |} data;
|};

type FraudResult record {
    string fraudlabspro_status;
};

type OrderCreated record {|
    int orderId;
    decimal total;
    string priority;
|};

final graphql:Client pricingClient = check new ("http://pricing:8080/graphql",
    retryConfig = {count: 2, interval: 0.2, statusCodes: [http:STATUS_SERVICE_UNAVAILABLE]});
final fraud:Client fraudClient = check new (serviceUrl = "http://fraud:8080");
final mistral:ModelProvider model = check new ("mistral-test-key", mistral:MISTRAL_SMALL_LATEST, serviceUrl = "http://mistral:8080/v1");
final postgresql:Client db = check new (host = "postgres", username = "shop", password = "shop-pass", database = "shop", port = 5432);

const CORRELATION_HEADER = "X-Correlation-Id";
const TOKEN_HEADER = "X-Api-Token";
const TIMING_HEADER = "X-Processing-Time-Ms";
const API_TOKEN = "orders-secret";
const CTX_CORRELATION = "correlationId";
const CTX_START = "startedAt";
const MILLIS_PER_SECOND = 1000;

service class CorrelationInterceptor {
    *http:RequestInterceptor;

    resource function 'default [string... path](http:RequestContext ctx, http:Request request)
            returns http:NextService|error? {
        string id = request.getHeader(CORRELATION_HEADER) is string ? check request.getHeader(CORRELATION_HEADER) : uuid:createType4AsString();
        ctx.set(CTX_CORRELATION, id);
        ctx.set(CTX_START, time:monotonicNow());
        return ctx.next();
    }
}

service class TokenInterceptor {
    *http:RequestInterceptor;

    resource function 'default [string... path](http:RequestContext ctx, http:Request request)
            returns http:NextService|http:Response|error? {
        string|http:HeaderNotFoundError token = request.getHeader(TOKEN_HEADER);
        if token is string && token == API_TOKEN {
            return ctx.next();
        }
        return errorResponse(http:STATUS_UNAUTHORIZED, "UNAUTHORIZED", "Missing or invalid API token");
    }
}

service class StampInterceptor {
    *http:ResponseInterceptor;

    remote function interceptResponse(http:RequestContext ctx, http:Response response)
            returns http:NextService|error? {
        string id = check ctx.getWithType(CTX_CORRELATION);
        decimal started = check ctx.getWithType(CTX_START);
        response.setHeader(CORRELATION_HEADER, id);
        response.setHeader(TIMING_HEADER, ((time:monotonicNow() - started) * MILLIS_PER_SECOND).round().toString());
        return ctx.next();
    }
}

service class FailureInterceptor {
    *http:ResponseErrorInterceptor;

    remote function interceptResponseError(error err, http:RequestContext ctx) returns http:Response {
        log:printError("request failed", err);
        if err is http:PayloadBindingError {
            return errorResponse(http:STATUS_BAD_REQUEST, "INVALID_REQUEST", err.message());
        }
        return errorResponse(http:STATUS_INTERNAL_SERVER_ERROR, "INTERNAL_ERROR", "Unexpected failure");
    }
}

function errorResponse(int status, string code, string message) returns http:Response {
    http:Response response = new;
    response.statusCode = status;
    response.setJsonPayload({code, message});
    return response;
}

service http:InterceptableService / on new http:Listener(8091) {
    public function createInterceptors() returns http:Interceptor|http:Interceptor[] {
        return [new CorrelationInterceptor(), new StampInterceptor(), new TokenInterceptor(), new FailureInterceptor()];
    }

    resource function post orders(@http:Payload json payload)
            returns http:Created|http:Response|error {
        OrderRequest|error parsed = payload.cloneWithType();
        if parsed is error || !isValid(parsed) {
            return errorResponse(http:STATUS_BAD_REQUEST, "INVALID_REQUEST", "Invalid order");
        }
        OrderRequest request = parsed;

        PricingResponse|error priced = pricingClient->query(PRICING_QUERY,
            variables = {id: request.customerId, skus: from OrderItem item in request.items select item.sku},
            headers = {"Authorization": "Bearer pricing-token"});
        if priced is error {
            return errorResponse(http:STATUS_BAD_GATEWAY, "UPSTREAM_ERROR", "Pricing is unavailable");
        }
        PricingResponse pricing = priced;
        Customer? customer = pricing.data.customer;
        map<float> unitPrices = {};
        foreach Price price in pricing.data.prices {
            unitPrices[price.sku] = price.price;
        }
        if customer is () {
            return errorResponse(http:STATUS_UNPROCESSABLE_ENTITY, "UNPROCESSABLE", "Order cannot be processed");
        }
        decimal subtotal = 0d;
        foreach OrderItem item in request.items {
            float? unitPrice = unitPrices[item.sku];
            if unitPrice is () {
                return errorResponse(http:STATUS_UNPROCESSABLE_ENTITY, "UNPROCESSABLE", "Order cannot be processed");
            }
            subtotal += <decimal>unitPrice * <decimal>item.qty;
        }
        decimal total = decimal:round(customer.tier == GOLD ? subtotal * GOLD_DISCOUNT : subtotal, 2);

        string screened = check fraudClient->screenOrder(request.ip, 'key = "fraud-test-key", format = "json");
        FraudResult fraudResult = check value:fromJsonStringWithType(screened);
        if fraudResult.fraudlabspro_status == REJECT {
            return errorResponse(http:STATUS_UNPROCESSABLE_ENTITY, "UNPROCESSABLE", "Order cannot be processed");
        }

        string priority = check prioritize(request.notes);

        int|error orderId = store(request, unitPrices, total, priority, fraudResult.fraudlabspro_status);
        if orderId is error {
            return errorResponse(http:STATUS_CONFLICT, "CONFLICT", "Order could not be stored");
        }
        OrderCreated body = {orderId, total, priority};
        http:Created created = {body};
        return created;
    }
}

function isValid(OrderRequest request) returns boolean {
    if request.customerId.trim() == "" || request.ip.trim() == "" || request.items.length() == 0 {
        return false;
    }
    foreach OrderItem item in request.items {
        if item.sku.trim() == "" || item.qty <= 0 {
            return false;
        }
    }
    return true;
}

function prioritize(string? notes) returns string|error {
    if notes is () {
        return NORMAL;
    }
    ai:ChatAssistantMessage reply = check model->chat({
        role: ai:USER,
        content: string `Must this order be expedited? Answer with exactly one word: HIGH if it must be expedited, otherwise NORMAL.

Order notes: ${notes}`
    }, tools = []);
    string answer = (reply.content ?: "").trim().toUpperAscii();
    return answer.startsWith(HIGH) ? HIGH : NORMAL;
}

function store(OrderRequest request, map<float> unitPrices, decimal total, string priority, string fraudStatus)
        returns int|error {
    transaction {
        int orderId = check db->queryRow(`INSERT INTO orders (customer_id, total, priority, fraud_status)
            VALUES (${request.customerId}, ${total}, ${priority}, ${fraudStatus}) RETURNING id`);
        sql:ParameterizedQuery[] lines = from OrderItem item in request.items
            select `INSERT INTO order_lines (order_id, sku, qty, unit_price)
                VALUES (${orderId}, ${item.sku}, ${item.qty}, ${<decimal>(unitPrices[item.sku] ?: 0.0f)})`;
        _ = check db->batchExecute(lines);
        check commit;
        return orderId;
    } on fail error failure {
        return failure;
    }
}
