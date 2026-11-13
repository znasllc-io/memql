# The fixture store

`orders.json` is the orders of an invented bicycle shop, written as the Admin
API's `ShopifyListOrder` answers them: the `data` of one list of orders, each
order with its lines in `lineItems.nodes` and its refunds in `refunds`, each
refund with its three inlined connections. It holds only fields the mirror's
order documents select, and of a refund's three connections only the fields
the mirror keeps. The shop, its buyers, their emails, its products and every
amount are made up: nothing in the file is a real shop's, person's, domain's
or product's. Emails end in `example.test`.

The shop sells in euros and keeps its days in `Europe/Madrid`. Its prices do
not include tax, except on the one order that says so.

`reference_db_test.go` writes the file through the mirror's own write path
and asks the store's figures for every period at 12:30 on Monday
10 November 2025, Madrid time. `reference_calc_test.go` computes the same
figures from this file alone, and the test requires the two to agree to the
cent. `TestTheFixtureHoldsEveryCaseTheRecordNames` holds every order below to
what it is for: the twenty cases the record names, and three a review added.

## The orders

Each order's `name` says what it is for; the tests find each case by it.

| Order | What it is for |
| --- | --- |
| `two-discount-codes` | Two codes on one order, and a line that carries an allocation of each. |
| `order-level-discount` | A discount of the whole order, allocated to its lines, one of which has no product and is grouped by its title. |
| `gift-card` | A gift card bought beside a pump and then refunded: no sale, and its refund no refund of items. |
| `line-refund-with-and-without-restock` | One unit of each of two lines refunded, in two refunds. The record's case is one refund restocked and one not, but the file cannot tell them apart: Shopify says so in a refund line's `restockType`, which the mirror does not keep, so it cannot move a figure. What the file holds is what the record says happens either way: each line's `currentQuantity` is one lower, and the refunded unit is restored as sold. |
| `shipping-refund` | Only the shipping refunded: no refund of items. |
| `amount-only-refund` | Money refunded with no line on it, recorded as an order adjustment with a tax part. |
| `restocking-fee` | Both units returned, less a restocking fee and the fee's tax. |
| `refund-in-a-later-period` | An October order refunded in November: the refund is on its own day. |
| `edit-removes-a-unit` | An edit left two of three units, with no refund. |
| `edit-adds-a-line` | An edit added a line (the tubes); a guest's order. In the file the order is `edited: true` and nothing else marks the added line: Shopify writes it as a line like any other, with its own `quantity`. That is the record's rule (5.4): an added line is counted on the order's day as any line, so no figure can tell it from an original one, and none should. |
| `cancelled-with-refund` | A cancelled order with a refund: neither counts. |
| `test-order` | A test order on a live store: it does not count, and its buyer's first counted order is a later one. |
| `order-in-usd` | An order in another currency: counted in Orders and Units sold, summed in no amount. |
| `three-orders-of-one-buyer-1` to `-3` | One buyer's three orders, in March, September and November: returning in the later periods; the oldest is the oldest order that counts. |
| `wholesale-buyer` | A wholesale buyer's order, its email written with capitals and spaces, and a partial refund on the wholesale side. |
| `either-side-of-a-local-midnight` | An order a moment before midnight in Madrid and its refund a moment after: one day in UTC, two store days. |
| `one-hundred-twenty-lines` | 120 lines, more than the list's page of 100, so the second page is read. |
| `discount-written-short` | Three units of which an edit left two, with a discount written `1.0`: its share is `0.67`. |
| `tax-inclusive-shipping-refund` | Prices that include tax, and the shipping refunded with its tax inside the subtotal. |
| `deleted-order` | Read, then retired by an `ORDERS_DELETE` notice: in no figure. |
| `duties-with-a-partial-duties-refund` | Duties of 12.00 charged on the order, and a refund of its helmet that gives 4.00 of them back: U(O) in Total sales on the order's day, RU(F) out of it on the refund's. |
| `processed-after-it-was-created` | Created late on Sunday 2 November and processed on Tuesday 4 November: the processing date places it (design D2), in another week than its creation. |
| `cancelled-before-the-first-counted-order` | A cancelled order older than every order that counts: in no figure, but it is the store's oldest fact, so the store's recorded history begins on its day (design 6.2, `history_since`). |

## The order of 120 lines

`one-hundred-twenty-lines` is laid out so it can be edited by hand. Line `i`,
counting from 0, is `gid://shopify/LineItem/4025` plus `i`. Its product cycles
through six, by `i` modulo 6, each always at its own price:

| `i` mod 6 | Product | Unit price |
| --- | --- | --- |
| 0 | Inner tube (`Product/502`) | 6.50 |
| 1 | Bell (`Product/503`) | 12.00 |
| 2 | Pump (`Product/508`) | 18.00 |
| 3 | Lights set (`Product/505`) | 24.90 |
| 4 | Lock (`Product/506`) | 33.00 |
| 5 | Saddle (`Product/507`) | 39.95 |

Its quantity is 1 plus `i` modulo 4, its current quantity the same, and it
carries no discount. The order's `subtotalLineItemsQuantity` is the sum of the
quantities, 300; a line added or removed by hand changes it too, or the
order reads as one whose lines were not all recorded. The fake Shopify in the
test cuts the list at 100 lines, as the document reads them, and answers the
rest as the next page.

## A change to a rule

A rule of the figures lives in four places, and they move together: the
record's section 5, the component, the calculator in `reference_calc_test.go`,
and this file. The record is changed first and says why; the component and the
calculator are each changed from the record's words, never one from the other;
and the orders here that the rule touches are changed to what Shopify writes,
with this README. The test then requires the two sides to agree again. A
difference between them is a finding about one of them, not something to be
made to match.

## If a live check finds otherwise

Section 5.2 of the design rests on two readings of Shopify that live checks
settle. Three orders here are written on those readings and change with them.

- **An order adjustment signed the other way** (L4, L12). The file writes a tax
  that went back to the buyer as a positive `taxAmountSet`, and a restocking
  fee as a negative `amountSet` with a negative `taxAmountSet`.
  - `amount-only-refund`: its adjustment's `amountSet` `8.26` and
    `taxAmountSet` `1.74` become `-8.26` and `-1.74`.
  - `restocking-fee`: its adjustment's `-9.00` and `-1.89` become `9.00` and
    `1.89`.

  The calculator then sums an adjustment's tax with its sign reversed, as the
  corrected formula will.
- **A tax-inclusive shipping refund written without its tax** (L13).
  - `tax-inclusive-shipping-refund`: its refund shipping line's
    `subtotalAmountSet` `4.95` becomes `4.09`, the amount without its `0.86`
    of tax.

  The calculator then takes the refunded shipping with its tax out of the
  money refunded, as the corrected formula will.

In each case the file, the record's formula and the calculator change
together, and the two sides must agree again.
