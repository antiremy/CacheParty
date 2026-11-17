// Run: node test-dashboard.cjs /path/to/generated/app.js
const assert = require('node:assert/strict');
const vm = require('node:vm');

module.exports = function verify(source) {
  new vm.Script(source);
  class Element {
    constructor(tag) { this.tag = tag; this.children = []; this.dataset = {}; this.style = {}; this.handlers = {}; }
    appendChild(child) { this.children.push(child); return child; }
    append(...children) { this.children.push(...children); }
    addEventListener(event, handler) { this.handlers[event] = handler; }
  }
  let purged;
  const context = vm.createContext({
    fetch: async () => ({ ok: false, status: 503 }), setTimeout: () => 0,
    document: { createElement: (tag) => new Element(tag) },
    settings: { editable: true }, fmtBytes: (bytes) => String(bytes),
    openPurge: (depot) => { purged = depot; },
  });
  new vm.Script(source.split('// --- formatting')[0]).runInContext(context);
  const evaluate = (script) => new vm.Script(script).runInContext(context);
  evaluate(`STEAM_APPS = { '101': '100', '102': '100', '201': '200' };
    var raw = {service:'steam', apps:[
      {id:'101',kind:'depot',sent:100,fetched:80,sentRate:3,fetchedRate:2,lastActive:10},
      {id:'102',kind:'depot',sent:50,fetched:20,sentRate:2,fetchedRate:1,lastActive:30},
      {id:'201',kind:'depot',sent:80,fetched:10,sentRate:0,fetchedRate:0,lastActive:20},
      {id:'999',kind:'depot',sent:1,fetched:1,sentRate:0,fetchedRate:0,lastActive:1}
    ]};
    var before = JSON.stringify(raw);
    var rows = groupedApps(raw);
    var game = rows.find(row => row.groupedAppId === '100');`);
  assert.equal(evaluate('rows.length'), 3);
  assert.deepEqual(JSON.parse(evaluate('JSON.stringify([game.sent,game.fetched,game.sentRate,game.fetchedRate,game.lastActive])')),
    [150, 100, 5, 3, 30]);
  assert.equal(evaluate('JSON.stringify(raw) === before'), true);
  assert.equal(evaluate('rows.some(row => row.id === "999" && !row.groupedAppId)'), true);
  assert.equal(evaluate('groupedApps({...raw,service:"blizzard"}) === raw.apps'), true);
  assert.equal(evaluate('depotURL(game.id)'), 'https://steamdb.info/app/100/');
  // Equal labels belonging to different apps must remain separate games.
  evaluate('GAME_NAMES = {steam:{depot:{"101":"Same title","102":"Same title","201":"Same title"}}};');
  assert.equal(evaluate('groupedApps(raw).filter(row => row.groupedAppId).length'), 2);
  const cell = new Element('td');
  context.cellForTest = cell;
  evaluate('renderDepotDetails(cellForTest, game);');
  const details = cell.children[0];
  assert.equal(details.children[0].textContent, '2 depots');
  const list = details.children[1];
  assert.equal(list.children.length, 2);
  list.children[1].children.find(child => child.tag === 'button').handlers.click();
  assert.equal(purged.id, '102'); // Group actions never purge only a representative depot.
  assert.equal(evaluate('JSON.stringify(raw) === before'), true);
  details.open = true;
  details.handlers.toggle();
  const next = new Element('td');
  context.nextCell = next;
  evaluate('renderDepotDetails(nextCell, game);');
  assert.equal(next.children[0].open, true); // Polling preserves expanded details.
  evaluate('settings.editable = false;');
  const readOnly = new Element('td');
  context.readOnlyCell = readOnly;
  evaluate('renderDepotDetails(readOnlyCell, game);');
  assert.equal(readOnly.children[0].children[1].children.some(item => item.children.length), false);
  return { grouping: 'passed', totals: 'passed', unknowns: 'passed',
    appLinks: 'passed', depotPurgeIdentity: 'passed', expandedState: 'passed' };
};

if (require.main === module) {
  const source = require('node:fs').readFileSync(process.argv[2], 'utf8');
  console.log(module.exports(source));
}
