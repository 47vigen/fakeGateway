# fakeGateway

Fake Iranian payment gateways for end-to-end tests: Bank Mellat (behpardakht),
Digipay, GSMPay, Kamanlend, Moneysa, SetareYek and Vibe. One binary and one
image. `GATEWAY` chooses the vendor a process plays, and the process serves
that vendor's real paths, so your driver only needs its base URL changed.

> **فارسی:** این پروژه نسخه‌ی جعلی (fake) درگاه‌های پرداخت ایرانی برای تست
> end-to-end است: به‌پرداخت ملت، دیجی‌پی، جی‌اس‌ام‌پی، کمان‌لند، مانیسا،
> ستاره‌یک و وایب. هر درگاه همان مسیرها، بدنه‌ها و کدهای خطای درگاه واقعی را
> جواب می‌دهد، با یک صفحه‌ی پرداخت که دکمه‌ی «پرداخت» و «انصراف» دارد. فقط
> آدرس پایه‌ی درایورتان را عوض کنید. یک API کنترلی هم دارد که تست خودکار با آن
> سفارش را موفق یا ناموفق می‌کند، کد خطا تحمیل می‌کند و تأخیر می‌سازد.

**These are test doubles.** State lives in memory and a restart forgets it.
Never point one at real money. The control API under `/api/v1/test/` has
**no authentication**: anyone who can reach it can settle or fail any order.
Keep it on a private network, or accept that it is open.

## Why trust the contracts

Each fake was written against a driver that talks to the live gateway in
production, not only against the vendor's documentation. Where the two
disagree, the fake does what production does, and the vendor's section says
so (Moneysa is the sharpest case). Where neither settles a detail, the section
says the fake guessed.

## Quick start

```sh
docker run --rm -p 8093:8090 -e GATEWAY=behpardakht \
  -e PUBLIC_BASE_URL=http://localhost:8093 ghcr.io/47vigen/fakegateway

docker compose -f deploy/compose.yaml up      # all seven, on ports 8092-8098

GATEWAY=vibe go run .                          # from a checkout, on :8090
go install github.com/47vigen/fakeGateway@latest
```

An empty or unknown `GATEWAY` exits with code 2 and lists the vendors.

| `GATEWAY` | Vendor | Kind | Beyond create and verify | Protocol |
|---|---|---|---|---|
| `behpardakht` | Bank Mellat | IPG (bank card) | settle, reversal | SOAP, browser POST-back |
| `digipay` | Digipay UPG | CPG (credit) | refund, deliver | REST, OAuth2 password grant, browser POST-back |
| `gsmpay` | GSMPay | CPG | refund (reverse) | REST, `merchant_code` in the body |
| `kamanlend` | Kamanlend | CPG | pre-check | REST, terminal code and secret in the body |
| `moneysa` | Moneysa | CPG | refund, inquiry | REST, username and password in the body |
| `setareyek` | SetareYek | BPG (buy now, pay later) | inquiry, refund | REST, bearer from `GetToken`, `Result` codes on HTTP 200 |
| `vibe` | Vibe | CPG | — | REST, `X-API-Key` |

## Why one vendor per process

A real gateway serves fixed paths at the root of its own host, and a driver is
configured with a base URL, not a prefix. Two vendors in one process would
fight over paths such as `/pay/{id}`. So you run one container per vendor, all
from the same image.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `GATEWAY` | (required) | The vendor this process plays. |
| `LISTEN_ADDR` | `:8090` | Listen address. |
| `PUBLIC_BASE_URL` | `http://localhost:8090` | The address the shopper's **browser** opens. Payment URLs contain it. A server-to-server caller may use another address. |
| `DEFAULT_OUTCOME` | `manual` | `manual`, `settle` or `fail`. |
| `ENFORCE_AMOUNTS` | `true` | `false` accepts a create whose total does not add up, where the vendor has such a rule. |

Each vendor reads its own credentials. They guard nothing; they exist so a
test can cause the vendor's wrong-credential answer.

## The payment page

Every vendor's redirect lands on one shared page (Persian, right-to-left) that
shows the amount and the basket with two buttons, pay and cancel. Either one
moves the order and sends the browser back to your callback in the vendor's
own way: a GET redirect, or a form the browser posts.

## The control API

Everything under `/api/v1/test/` belongs to the fake; no real gateway has it.
`{id}` is the vendor's own reference, named in its section.

```sh
curl -s localhost:8090/api/v1/test/orders                     # {"count":n,"orders":[…]}, oldest first
curl -s localhost:8090/api/v1/test/orders/$ID                 # one order, its ledger, its create request
curl -s -XPOST localhost:8090/api/v1/test/orders/$ID/settle   # {"order":…}
curl -s -XPOST localhost:8090/api/v1/test/orders/$ID/fail
curl -s -XPOST "localhost:8090/api/v1/test/orders/$ID/settle?callback=1"   # {"order":…,"callback":…}
curl -s -XPOST localhost:8090/api/v1/test/orders/$ID/callback # the browser returns, the state stays
curl -s -XPOST localhost:8090/api/v1/test/reset               # {"cleared":n}
curl -s localhost:8090/api/v1/test/config
curl -s -XPOST localhost:8090/api/v1/test/config -d '{"force":{"verify":"34"},"delay_ms":0}'
```

- **A settle does not fire the callback.** The order's state is what verify
  reports; the callback is a network call to your service. A test of verify
  should not need your service up. Add `?callback=1` for both.
- **A fired callback returns your service's answer, unfollowed**:
  `{"url","fields","status","location"}`, `{"dropped":true}` or `{"error"}`.
  The `Location` is usually what a test asserts.
- **Each order keeps a ledger**: a count of each provider operation, and the
  create request as it arrived. It is the independent witness. Assert what the
  gateway was actually asked, not what your service says it asked.

### Knobs

`POST /api/v1/test/config` merges what you send into the current knobs; `{}`
restores the startup defaults. The answer is the full set.

| Field | Meaning |
|---|---|
| `default_outcome` | `manual`, `settle` or `fail`. Anything but `manual` puts each new order straight into that state, without a callback. |
| `delay_ms` | Delays each provider operation. Use it to cause a client timeout. |
| `drop_callback` | The control API sends no callback: the money moves and the browser never returns. This is the case reconciliation exists for. |
| `enforce_amounts` | As the environment variable. |
| `force` | `{"<operation>": "<code or status>"}`. That operation answers this value whatever the order's state. `""` or `"0"` clears it. The operations and the meaning of a value are in each vendor's section. |

Older names still work and write into `force`: `pay_code`, `verify_code`,
`settle_code` (behpardakht), `create_status`, `verify_status` (vibe).

## behpardakht

Bank Mellat's internet payment gateway. `{id}` is the `RefId`.

| Path | What |
|---|---|
| `/pgwchannel/services/pgw?wsdl` | The WSDL. |
| `/pgwchannel/services/pgwverify?wsdl` | The same document. |
| `POST` on either | `bpPayRequest`, `bpVerifyRequest`, `bpSettleRequest`, `bpReversalRequest`. |
| `/pgwchannel/startpay.mellat?RefId=…` | The payment page (`/pgwchannel/startpay` too). |
| `POST /pgwchannel/pay` | The page's buttons. The browser then posts `RefId`, `ResCode`, `SaleOrderId`, `SaleReferenceId`, `CardHolderPan` to `callBackUrl`, as the bank does. |

**The WSDL gives the address it was fetched from**, not `PUBLIC_BASE_URL`.
node-soap POSTs to `<soap:address>`, so a caller inside a docker network must
get the in-network address, or it calls its own `localhost`. The scheme
follows `X-Forwarded-Proto`, and a request for `PUBLIC_BASE_URL`'s own host
takes that URL's scheme, so the address stays https behind a TLS-terminating
proxy. node-soap also caches a WSDL in its process: restart the client after
the address changes.

| Code | Meaning | Why it matters |
|---|---|---|
| `0` | success | |
| `43` | already verified | Treat it as success, or a verify retry reverses a captured payment. |
| `45` | already settled | The same: the money moved. |
| `48` | reversed | Must keep failing. Crediting it creates money. |
| `12`, `41`, `42`, `34` | insufficient funds, duplicate orderId, not found, system error | The refusals. |

- `USERNAME` (`fake-terminal`), `PASSWORD` (`fake-password`). Wrong ones answer
  `34`. The terminal id is not checked.
- Forced operations: `pay`, `verify`, `settle` answer that result code.
- Ledger: `pay_calls`, `verify_calls`, `settle_calls`, `reversal_calls`,
  `callback_calls`. `request` is the `bpPayRequest`.

## digipay

Digipay's unified payment gateway. `{id}` is the ticket (`v2:…`); Digipay's
`trackingCode` appears on the callback and in the order view. A token comes
from an OAuth2 password grant with the client in a Basic header; every other
provider route needs `Authorization: Bearer <access_token>`, and a missing or
unknown one answers `401 {"error":"invalid_token"}`.

| Path | What |
|---|---|
| `POST /oauth/token` | `grant_type=password` or `refresh_token`. Answers `{access_token,refresh_token,expires_in,…}`. A wrong client is `401`, a wrong user or refresh token `400 invalid_grant`. |
| `POST /tickets/business?type=11` | Create. Answers `{result,ticket,redirectUrl:"<PUBLIC_BASE_URL>/web-pay/tgs/<ticket>"}`. |
| `POST /purchases/verify?type=<purchase type>` | Verify `{trackingCode,providerId}`. Credit purchases add `fpCode`, `fpName`, `additionalInfo`; others add `rrn`, `maskedPan`, `psp*`. |
| `POST /purchases/deliver?type=` | Deliver `{trackingCode,deliveryDate,invoiceNumber,products}`. Credit purchases (5, 13) only. |
| `POST /refunds?type=` | Refund `{providerId,amount,saleTrackingCode}`. The `providerId` must differ from the purchase's. |
| `GET /web-pay/tgs/{ticket}` | The payment page. Its buttons post `result=SUCCESS\|FAILURE`, `providerId`, `trackingCode`, `amount`, `type`, `isCredit` (and `rrn` for IPG) to `callbackUrl`. |

- Success is `result.status: 0`. A refusal is `422 {"result":{"status",…}}`,
  except `1054` (bad input, a wrong `?type`), which is `400`. `9000` unknown
  code, `9004` not yet paid, `9007` failed, `9008` `providerId` already used,
  `9012` already verified, refunded or delivered, or not verified.
- `PURCHASE_TYPE` (`5`) is the method every shopper pays with: 0 IPG, 5
  CREDIT, 11 WALLET, 13 BNPL, 24 CREDIT-CARD. Verify, deliver and refund must
  name the same type.
- `CLIENT_ID` (`fake-client`), `CLIENT_SECRET` (`fake-secret`), `USERNAME`
  (`fake-user`), `PASSWORD` (`fake-password`).
- Forced operations: `token`, `create`, `verify`, `deliver`, `refund`. A value
  from 100 to 599 is that HTTP status; anything else is a `result.status`.
- Ledger keys: `create`, `verify`, `deliver`, `refund`, `callback`.
- Guessed, because nothing settles them yet: `1054` for a wrong `?type`,
  `9012` for a re-verify, `9004`, `9000` and `9008` where used, and the OAuth
  error bodies. The `Agent` and `Digipay-Version` headers the swagger requires
  are not required, because production clients do not send them.

## gsmpay

GSMPay's credit gateway. `{id}` is the `token`. No header and no token: every
body carries `merchant_code` (`MERCHANT_CODE`, default `fake-merchant`); a
wrong one answers `422 {"type":"merchant_code_error"}`. GSMPay has no 401.

| Path | What |
|---|---|
| `POST /v1/cpg/payments` | Create. Answers `201 {"data":{"token","redirect_url":"<PUBLIC_BASE_URL>/pay/<token>"}}`. |
| `POST /v1/cpg/payments/verify` | `{merchant_code,token,invoice_reference,invoice_amount}`. Answers `{"data":{"token","invoice_amount","is_paid"}}`. |
| `POST /v1/cpg/payments/reverse` | The refund: `{merchant_code,token,reverse_amount}`. Partial reverses add up. Answers `{"data":{"token","reversed_amount"}}`, the running total. |
| `GET /pay/{token}` | The payment page. Its buttons redirect to `callback_url?token=<token>&status=success\|failed`. |

- Errors are `{"type":"<code>"}`, 422 unless noted: `validation_error` (with
  `errors`, also for a duplicate `invoice_reference`), `merchant_code_error`,
  `payment_token_error`, `invoice_amount_error`, `invalid_order_status`
  (reverse before payment), `reverse_amount_exceeded`, `authorization_error`
  (403), `server_error` (500).
- Verify can be repeated: GSMPay documents no "already verified". `is_paid` is
  true only while the payment is settled and not fully reversed.
- Forced operations: `create`, `verify`, `refund`. The value is an error type,
  or an HTTP status that answers `server_error`.
- Ledger keys: `create`, `verify`, `refund`, `callback`.
- Guessed: the `status` value on a failed return.

## kamanlend

Kamanlend's lending gateway. `{id}` is the `token`. Every body carries
`terminalCode` and `terminalSecret` (`TERMINAL_CODE`, `TERMINAL_SECRET`;
defaults `fake-terminal`, `fake-secret`); a wrong pair answers 401. Every
answer is `{"success","messages":[{"messageKind","message","messageDate"}],"result"}`,
and a refusal is HTTP 400 with `success:false` and one Persian `Error` message,
as the real one sends.

| Path | What |
|---|---|
| `POST /api/Gateway/RegisterPayment` | Registers a sale. Answers `result:{token, gatewayUrl:"<PUBLIC_BASE_URL>/pay/<token>"}`. |
| `POST /api/Gateway/GetPaymentState` | `{terminalCode,terminalSecret,customerNationalCode,token}`. Answers `result:{totalSaleAmountRial,cashPaymentAmountRial,saleRequestState,…}`. |
| `GET /pay/{token}` | The payment page. Its buttons redirect to `redirectionUrl?token=<token>`. |

- **A pre-check is a register.** Kamanlend has no eligibility call, so a
  client probes it by registering a sale and discarding the redirect. Each
  register checks the amount (the sum of `saleItems[].totalAmountRial`)
  against the shopper's credit:
  - `CREDIT_LIMIT` sets it (default 1,000,000,000 Rial);
  - `force.pre_check: "not_eligible"` refuses every register;
  - `force.pre_check: "<rial>"` replaces the limit (use `"1"`, not `"0"`,
    which clears the force).
- States: pending is `WaitForCustomerApprove`, settled `PaymentCompleted`,
  failed `Canceled`.
- An unknown token, or a national code that is not the sale's, answers 400. A
  pending `shoppingCardCode` can be registered again; a paid one is refused.
- Forced operations: `create` (an HTTP status), `verify` (an HTTP status or a
  state name such as `Rejected`), `pre_check` (above).
- Ledger keys: `create` (every register, pre-checks included), `verify`,
  `callback`.
- Guessed: the return's query parameters.

## moneysa

Moneysa's credit gateway. `{id}` is the numeric `transaction_id` (from
1855601); its `token` is a separate value. get-token and refund carry
`username` and `password` in the body (`USERNAME`, `PASSWORD`; defaults
`fake-username`, `fake-password`); a wrong pair answers
`401 {"message","code":1011}`.

**This fake follows production, not the swagger:**

- An error body is `{"message","code"}`, not `{error_code,error_message}`.
- A paid transaction verifies as `status: 10`, `"verified"`, «پرداخت شده».
  The swagger's 1/0/3/2/-1 enum is not what the service answers.
- An unpaid one is HTTP 400 code `2002`.
- A second verify is HTTP 400 code `2001`, «تراکنش قبلا تسویه شده.», not a 200.

| Path | What |
|---|---|
| `POST /api/payments/get-token` | Answers `{tracking_code,transaction_id,url_meta,token,url:"<PUBLIC_BASE_URL>/pay/<transaction_id>"}`. |
| `POST /api/payments/verify` | `{token,transaction_id,amount}`. Succeeds once, on a paid transaction. |
| `POST /api/payments/verify-information` | The same request. Reports `status` 10 (paid), 3 (refunded), 2 (pending) or 0 (failed). |
| `POST /api/payments/refund` | `{username,password,token,transaction_id,amount}`. Answers `status: 3`, `"refunded"`. |
| `GET /pay/{transaction_id}` | The payment page. Its buttons redirect to `callback?token=<token>`. |

- Codes: 1001 not found (404), 1003 wrong token, 1011 credentials (401), 2001
  already verified, 2002 wrong state (unpaid, failed or refunded), 2003 wrong
  amount, 2006 refund above the amount. Validation is
  `422 {"message","errors"}`, including a duplicate `tracking_code`.
- `transaction_id` is accepted as a number or a string; clients send both.
- One refund closes the transaction; partial refunds are not tracked.
- Forced operations: `create`, `verify`, `inquiry`, `refund`. The value is a
  Moneysa code or a three-digit HTTP status.
- Ledger keys: `create`, `verify`, `inquiry`, `refund`, `callback`.
- Guessed: statuses 3, 2 and 0 of verify-information come from the swagger
  only, and the return's query parameters.

## setareyek

SetareYek's buy-now-pay-later gateway. `{id}` is the `TrackId`.
`GET /GetToken?username=&password=` answers the token as a bare JSON string;
the other routes need `Authorization: Bearer <token>`, and a missing or unknown
one is a bare `401`.

| Path | What |
|---|---|
| `POST /Purchase` | Create. Answers `{TrackId,Url:"<PUBLIC_BASE_URL>/pay/<TrackId>",Result,Message}`. |
| `POST /Verify` | `{Merchant,TrackId}` → `{PaidAt,Status,Amount,Result,Message}`. |
| `POST /Inquiry` | `{Merchant,TrackId}` → `{CreatedAt,Status,Amount,Result,Message}`. |
| `POST /RefundPayment` | `{Merchant,TrackId,Amount,Orders}` → `{Result,Message,Status}`. |
| `GET /pay/{TrackId}` | The payment page. Its buttons redirect to `CallbackUrl?TrackId=…&Status=…`. |

- **Every business answer is HTTP 200.** `Result: 100` is success. `102` wrong
  `Merchant`, `106` amount too low, `107` bad callback URL, `108` no `Orders`,
  `113` refund above the loan, `115` unknown `TrackId`, `116` not paid.
- Refund `Status`: `0` done, `1` failed. A second refund answers
  `Result: 100, Status: 1`, so check `Status` before `Result`.
- Verify is idempotent: a verified loan answers 100 again; a refunded one 116.
- Inquiry `Status`: `1` created, `102` paid, `103` failed, `104` verified,
  `105` reversed, `106` refunded.
- `USERNAME` (`fake-user`), `PASSWORD` (`fake-password`), `MERCHANT`
  (`fake-merchant`; your configured merchant id must match it).
- Forced operations: `token`, `create`, `verify`, `inquire`, `refund`. A value
  from 100 to 199 is a `Result` in a 200 body; from 200 to 599 it is that HTTP
  status with no body.
- Ledger keys: `create`, `verify`, `inquire`, `refund`, `callback`.
- Guessed: the return's parameters, the 401 shapes and the already-refunded
  answer. SetareYek has no duplicate-create or already-verified code.

## vibe

Vibe's credit gateway. `{id}` is the `order_id`. Both provider routes need
`X-API-Key` (`API_KEY`, default `fake-key`); a wrong or missing key answers
`401 {"code":1001,"detail":"Invalid API key provided","data":null}`.

| Path | What |
|---|---|
| `POST /api/v1/orders/` | Create. Answers `{"payment_url":"<PUBLIC_BASE_URL>/pay/<order_id>"}`. |
| `POST /api/v1/orders/verify` | Verify `{"ref_id"}`. Answers `{"status","message"}`. |
| `GET /pay/{order_id}` | The payment page. Its buttons redirect to `callback_url?result=OK\|NOK&ref_id=<order_id>`. |

```sh
curl -s localhost:8090/api/v1/orders/ -H 'X-API-Key: fake-key' -d '{
  "order_id": "0198f3c1-1111-2222-3333-444455556666",
  "cart_amount": 12500000,
  "callback_url": "http://localhost:3000/callback/0198f3c1-1111-2222-3333-444455556666",
  "data": { "tax": 0, "goods_amount": 12500000,
    "items": [{ "id": "p1", "name": "Item", "price": 5000000, "quantity": 2, "discount": 0 },
              { "id": "p2", "name": "Other", "price": 2500000, "quantity": 1, "discount": 0 }] }
}'
```

- `cart_amount_installment` and `installments` are ignored.
  `422 {"detail":[{"loc":[…],"msg":"…","type":"value_error"}]}` answers a
  missing `order_id`, a `cart_amount` that is not positive, empty
  `data.items`, an `order_id` that already exists, and, with
  `enforce_amounts`, `cart_amount != data.goods_amount + data.tax`.
- **The trap this fake reproduces:** for an order it cannot find, or that is
  not settled, verify answers **`200` with `status: false`**, not 404.
- Vibe issues no token: the `order_id` you send is also the callback's
  `ref_id` and verify's `ref_id`.
- Forced operations: `create`, `verify` send that HTTP status in the
  provider's fault envelope.
- Ledger keys: `create`, `verify`, `callback`.

## Deploying

`deploy/compose.yaml` runs all seven. Behind a reverse proxy, give each vendor
its own host, point the host at the container's port 8090, and set the
vendor's `*_URL` variable to that public address so its payment page links are
right. A TLS-terminating proxy is fine: the behpardakht WSDL keeps https (see
its section). Pin a version with `FAKEGATEWAY_TAG` (a commit sha or a release).

## Adding a vendor

1. Make the package `vendors/<name>/` (`vendors/`, because Go reserves
   `vendor/`) and implement `core.Vendor`:
   - `Name()` is the `GATEWAY` value.
   - `Routes(mux, c)` registers the provider's real paths. Send the payment
     page to `c.PayPage(w, id, action, hidden)` and its form to
     `c.PaySubmit(w, r, id)`.
   - `Callback(o)` gives the URL and fields for the order's current state.
     `Post: true` makes the browser post a form; otherwise the fields ride in
     the query of a GET redirect.
   - `View(o)` is the order as the control API shows it.
2. Keep the vendor's own state in `core.Order.Extra` as a value type (`c.Do`
   changes it atomically with a lookup). Count each provider operation with
   `o.Count("<op>")`, call `Behaviour().Wait()` before it, and read a forced
   answer from `Behaviour().Force["<op>"]`.
3. Add it to `vendors()` in `main.go`.
4. Test it in the package against `core.Handler(New(), base, core.Behaviour{…})`.
5. Add a service to `deploy/compose.yaml`, a row to the table above, and a
   section here.

A contract change is welcome as a pull request, best with the evidence: the
request and the answer the real gateway gave.

## Images

`.github/workflows/image.yml` checks formatting and runs `go vet` and
`go test -race` on every push and pull request. On `main` it pushes
`ghcr.io/47vigen/fakegateway` for amd64 and arm64, tagged `latest` and with
the commit sha; a `v*` tag also pushes that version.

## License

[MIT](LICENSE).
