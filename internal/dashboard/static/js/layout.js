import { $ } from "./dom.js";

// The smaller cards are packed into as many columns as fit, each card going to
// the shortest column so far. A card stays in its column unless moving it
// evens things out by more than CARD_STICKY, so cards don't hop as tables grow.
const CARD_MIN_WIDTH = 380;
const CARD_STICKY = 48;
let cardBox, cards;
let cardCols = [];

function layoutCards() {
  const gap = parseFloat(getComputedStyle(cardBox).rowGap) || 0;
  const n = Math.max(1, Math.floor((cardBox.clientWidth + gap) / (CARD_MIN_WIDTH + gap)));
  if (cardCols.length !== n) {
    cardCols = Array.from({ length: n }, () => Object.assign(document.createElement("div"), { className: "masonry-col" }));
    cardBox.style.gridTemplateColumns = `repeat(${n}, minmax(0, 1fr))`;
  }
  const heights = cardCols.map(() => 0);
  const want = cardCols.map(() => []);
  for (const card of cards) {
    let col = heights.indexOf(Math.min(...heights));
    const cur = cardCols.indexOf(card.parentNode);
    if (cur >= 0 && heights[cur] - heights[col] <= CARD_STICKY) col = cur;
    want[col].push(card);
    if (!card.hidden) heights[col] += card.offsetHeight + gap;
  }
  // Moving a card blurs whatever was focused inside it, such as a link.
  const focused = document.activeElement;
  cardCols.forEach((col, i) => {
    if (want[i].length !== col.children.length || want[i].some((c, j) => col.children[j] !== c)) col.replaceChildren(...want[i]);
  });
  if (cardCols.some((c) => c.parentNode !== cardBox)) cardBox.replaceChildren(...cardCols);
  if (focused && focused !== document.activeElement) focused.focus({ preventScroll: true });
}

export function initLayout() {
  cardBox = $("cards");
  cards = [...cardBox.children];
  let frame = 0;
  const observer = new ResizeObserver(() => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(layoutCards);
  });
  observer.observe(cardBox);
  cards.forEach((c) => observer.observe(c));
  layoutCards();
}
