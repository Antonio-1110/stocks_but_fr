"""Issue #58: a sell on a day the stock doesn't trade waits for its next open."""

from .test_limits import D, FILL, SIGNAL, _flat, run


def test_sell_waits_out_a_halt(tmp_path):
    bars = {"1101": _flat(10.0), "0050": _flat(50.0)}
    sell_signal = SIGNAL + 5
    for i in (sell_signal + 1, sell_signal + 2):      # halted the two days after the sell signal
        del bars["1101"][D[i]]
    for i in range(sell_signal + 3, len(D)):
        bars["1101"][D[i]] = [12.0, 12.0, 12.0]
    res = run(tmp_path, bars, {SIGNAL: {"1101": 0.5}, sell_signal: {}})
    buys = [f for f in res.fills if f.side == "buy"]
    sells = [f for f in res.fills if f.side == "sell"]
    assert len(buys) == 1 and buys[0].day == D[FILL]
    assert len(sells) == 1 and sells[0].day == D[sell_signal + 3] and sells[0].price == 12.0
    assert res.skipped_orders == 0 and res.open_trades == []
    assert res.limit_blocked == 0


def test_partial_sell_waits_out_a_halt(tmp_path):
    bars = {"1101": _flat(10.0), "0050": _flat(50.0)}
    sell_signal = SIGNAL + 5
    del bars["1101"][D[sell_signal + 1]]
    res = run(tmp_path, bars, {SIGNAL: {"1101": 0.5}, sell_signal: {"1101": 0.25}})
    sells = [f for f in res.fills if f.side == "sell"]
    assert len(sells) == 1 and sells[0].day == D[sell_signal + 2]
    assert len(res.open_trades) == 1        # half of the position is still held


def test_buy_on_a_halt_is_still_skipped(tmp_path):
    bars = {"1101": _flat(10.0), "0050": _flat(50.0)}
    del bars["1101"][D[FILL]]
    res = run(tmp_path, bars, {SIGNAL: {"1101": 0.5}})
    assert res.fills == [] and res.skipped_orders == 1
