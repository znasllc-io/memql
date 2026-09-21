// storefront.js -- what the fixture bundle actually does (memql#5536).
//
// FIVE CHECKS, IN DEPENDENCY ORDER, EACH REPORTING ITS OWN READING. Read the
// runtime document; find the store binding in it; find the Storefront token
// beside it; call that store's Storefront API for a catalog; load one image
// from the URL the catalog gave back.
//
// THE POINT IS THE POLICY. Every one of the last two steps is a cross-origin
// request the edge's content security policy has to admit, and before
// memql#5534 it admitted neither -- so this file is the first thing in the
// tree that would have failed against a real cluster. A refusal by the
// policy surfaces here as a TypeError from fetch() with no status, which is
// why `halt` says what it says: a network-level failure on a cross-origin
// call is, far more often than not, the policy.
//
// NOTHING IS INVENTED. A check that has not run reads "not checked". A check
// that ran and found nothing reads what it found. The two are never the same
// words and never the same colour.

const API_VERSION = '2026-07';

const CATALOG_QUERY = `query FixtureCatalog {
  products(first: 3) {
    edges {
      node {
        title
        priceRange { minVariantPrice { amount currencyCode } }
        images(first: 1) { edges { node { url altText } } }
      }
    }
  }
}`;

/** The reading cell of one ledger row. */
function cell(check) {
  const row = document.querySelector(`.row[data-check="${check}"]`);
  return row ? { row, reading: row.querySelector('.reading') } : null;
}

/** Record a value this page actually read. */
function measured(check, text) {
  const c = cell(check);
  if (!c) return;
  c.reading.className = 'reading measured';
  c.reading.textContent = text;
  clearRemedy(c.row);
}

/**
 * Record a check that ran and refused, with the remedy in the row.
 *
 * `remedy` is not an apology and not a restatement of the reading: it is the
 * next thing to go and look at. A refusal that says only "failed" costs the
 * reader the whole investigation.
 */
function halted(check, text, remedy) {
  const c = cell(check);
  if (!c) return;
  c.reading.className = 'reading halted';
  c.reading.textContent = text;
  clearRemedy(c.row);
  if (remedy) {
    const p = document.createElement('p');
    p.className = 'remedy';
    p.textContent = remedy;
    c.row.appendChild(p);
  }
}

/** Return a row to "not checked" -- the state a re-run starts from. */
function unmeasured(check) {
  const c = cell(check);
  if (!c) return;
  c.reading.className = 'reading unmeasured';
  c.reading.textContent = 'not checked';
  clearRemedy(c.row);
}

function clearRemedy(row) {
  const existing = row.querySelector('.remedy');
  if (existing) existing.remove();
}

function money(price) {
  if (!price || price.amount === undefined || price.amount === null) return 'no price';
  const amount = Number(price.amount);
  if (Number.isNaN(amount)) return 'no price';
  return `${amount.toFixed(2)} ${price.currencyCode || ''}`.trim();
}

/** Render what came back, or say which kind of empty this is. */
function renderItems(products) {
  const evidence = document.getElementById('evidence');
  const list = document.getElementById('items');
  list.textContent = '';
  evidence.hidden = false;

  if (products.length === 0) {
    const p = document.createElement('p');
    p.className = 'empty';
    // THE TWO EMPTIES ARE DIFFERENT SENTENCES. This one is "the call
    // succeeded and the store has nothing published", which is a fact about
    // the store. "Not checked" is the other one, and it lives in the ledger.
    p.textContent = 'The store answered, and has no published products.';
    list.appendChild(p);
    return;
  }

  for (const product of products) {
    const li = document.createElement('li');
    li.className = 'item';

    const image = product.images?.edges?.[0]?.node;
    if (image?.url) {
      const img = document.createElement('img');
      img.src = image.url;
      img.alt = image.altText || '';
      img.loading = 'lazy';
      li.appendChild(img);
    } else {
      const held = document.createElement('span');
      held.className = 'noimage';
      li.appendChild(held);
    }

    const title = document.createElement('span');
    title.className = 'title';
    title.textContent = product.title || 'Untitled';
    li.appendChild(title);

    const price = document.createElement('span');
    price.className = 'price';
    price.textContent = money(product.priceRange?.minVariantPrice);
    li.appendChild(price);

    list.appendChild(li);
  }
}

/**
 * Load one image and report whether the bytes arrived.
 *
 * SEPARATE FROM THE CATALOG CHECK ON PURPOSE. The catalog is a connect-src
 * question and the image is an img-src one; they are different directives
 * and they failed independently before #5534. Folding them into one reading
 * would hide which of the two a cluster is missing.
 */
/** Where a URL came from, in the fewest words that stay true. */
function originLabel(url) {
  try {
    const u = new URL(url);
    return u.host || u.protocol.replace(':', '');
  } catch (_) {
    return 'an unreadable URL';
  }
}

function checkImage(url) {
  return new Promise((resolve) => {
    if (!url) {
      halted('image', 'no image',
        'The catalog came back with no image on its first product, so there was nothing to load. ' +
        'This is a fact about the store, not about the policy.');
      resolve(false);
      return;
    }
    const probe = new Image();
    probe.onload = () => {
      // NAME WHERE IT CAME FROM, and fall back to the SCHEME when there is no
      // host to name -- a data: URL has an empty host, and reporting "loaded
      // from " with nothing after it says less than saying nothing.
      measured('image', `loaded from ${originLabel(url)}`);
      resolve(true);
    };
    probe.onerror = () => {
      halted('image', 'refused',
        'The image did not load. Check that img-src in this page\'s Content-Security-Policy ' +
        'names the host the URL points at.');
      resolve(false);
    };
    probe.src = url;
  });
}

async function run() {
  const button = document.getElementById('recheck');
  button.setAttribute('aria-busy', 'true');
  button.disabled = true;
  for (const check of ['document', 'binding', 'token', 'catalog', 'image']) unmeasured(check);
  document.getElementById('evidence').hidden = true;

  try {
    // 1. The runtime document.
    let config;
    try {
      const res = await fetch('/runtime-config.json', { cache: 'no-store' });
      if (!res.ok) {
        halted('document', `HTTP ${res.status}`,
          'The edge answers GET /runtime-config.json for every live site. A non-200 here means ' +
          'this request did not reach the edge, or the site is not live.');
        return;
      }
      config = await res.json();
    } catch (err) {
      halted('document', 'unreachable',
        'GET /runtime-config.json did not complete. The page itself is being served, so this is ' +
        'the request rather than the site.');
      return;
    }
    measured('document', 'read');

    document.getElementById('served-at').textContent = location.host;

    // 2. The store binding. Its ABSENCE is a specific, diagnosable state:
    //    the block is omitted entirely for every kind but shopify_storefront,
    //    so "absent" means this site is not declared a storefront at all.
    const storefront = config.storefront;
    if (!storefront) {
      halted('binding', 'absent',
        'The runtime document carries no storefront block. The edge writes one only for a site ' +
        'whose kind is shopify_storefront -- check this deployable\'s kind.');
      document.getElementById('bound-to').textContent = 'nothing';
      return;
    }
    const storeDomain = (storefront.storeDomain || '').trim();
    if (!storeDomain) {
      halted('binding', 'no store',
        'The site is a storefront and its binding names no store. Set storeDomain on the ' +
        'deployable\'s binding.');
      document.getElementById('bound-to').textContent = 'nothing';
      return;
    }
    measured('binding', storeDomain);
    document.getElementById('bound-to').textContent = storeDomain;

    // 3. The token. Its LENGTH is reported and its VALUE never is: the
    //    Storefront token is public by Shopify's design, but printing a
    //    credential into a page anybody can screenshot is a habit worth not
    //    having.
    const token = (storefront.storefrontToken || '').trim();
    if (!token) {
      halted('token', 'empty',
        'The binding names a secret the edge could not resolve, or names none. The token is read ' +
        'from the v1:platform:globalSecret that storefrontTokenRef names, at serve time.');
      return;
    }
    measured('token', `present, ${token.length} chars`);

    // 4. The catalog -- the connect-src question.
    let payload;
    try {
      const res = await fetch(`https://${storeDomain}/api/${API_VERSION}/graphql.json`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Shopify-Storefront-Access-Token': token,
        },
        body: JSON.stringify({ query: CATALOG_QUERY }),
      });
      if (!res.ok) {
        halted('catalog', `HTTP ${res.status}`,
          `The store answered ${res.status}. The request reached ${storeDomain}, so the policy ` +
          'admits it; check the token and the API version.');
        return;
      }
      payload = await res.json();
    } catch (err) {
      // A cross-origin fetch refused by CSP fails here with no status at all.
      halted('catalog', 'refused',
        `The call to ${storeDomain} did not complete, and returned no status. On a cross-origin ` +
        'request that usually means connect-src in this page\'s Content-Security-Policy does not ' +
        'name the store.');
      return;
    }

    if (payload.errors?.length) {
      halted('catalog', 'GraphQL error',
        `The store answered with an error: ${payload.errors[0].message}`);
      return;
    }

    const products = (payload.data?.products?.edges || []).map((e) => e.node).filter(Boolean);
    measured('catalog', products.length === 1 ? '1 product' : `${products.length} products`);
    renderItems(products);

    // 5. One image -- the img-src question.
    await checkImage(products[0]?.images?.edges?.[0]?.node?.url);
  } finally {
    button.removeAttribute('aria-busy');
    button.disabled = false;
    const stamp = new Date().toTimeString().slice(0, 8);
    document.getElementById('note').textContent =
      `Checked at ${stamp}. Nothing on this page is cached; every check runs again when you do.`;
  }
}

document.getElementById('recheck').addEventListener('click', run);
run();
