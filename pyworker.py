import sys, json, time, hashlib, warnings

# One request per line on stdin, one JSON line on stdout. A request is
# {"setup": expr, "call": expr, "n": int, "iters": int} (time `iters` calls of
# eval(call)(*eval(setup)) inside this process, so no pipe is in the interval),
# {"setup": expr, "n": int, "hash": true} (SHA-256 of the arguments, to check
# that both sides time the same data) or {"versions": true}.
warnings.simplefilter("ignore")
import numpy as np

env = {"np": np}
for name, mod in (("sp", "scipy.special"), ("st", "scipy.stats"), ("sm", "statsmodels.api"), ("mp", "mpmath")):
    try:
        env[name] = __import__(mod, fromlist=["_"])
    except ImportError:
        pass

def versions():
    v = {"python": sys.version.split()[0], "numpy": np.__version__}
    for name, mod in (("scipy", "scipy"), ("statsmodels", "statsmodels"), ("mpmath", "mpmath")):
        try:
            v[name] = __import__(mod).__version__
        except ImportError:
            pass
    try:
        blas = np.show_config(mode="dicts")["Build Dependencies"]["blas"]
        v["blas"] = "%s %s" % (blas["name"], blas.get("version", ""))
    except Exception:
        pass
    return v

def digest(args):
    h = hashlib.sha256()
    for a in args:
        if isinstance(a, np.ndarray):
            h.update(np.ascontiguousarray(a).tobytes())
        else:
            h.update(repr(a).encode())
    return h.hexdigest()

cache = {}
for line in sys.stdin:
    try:
        c = json.loads(line)
        if c.get("versions"):
            out = {"versions": versions()}
        else:
            key = (c["setup"], c["n"])
            if key not in cache:
                cache[key] = eval(c["setup"], dict(env, n=c["n"]))
            args = cache[key]
            if c.get("hash"):
                out = {"hash": digest(args)}
            else:
                f = eval(c["call"], dict(env, n=c["n"]))
                iters = int(c["iters"])
                with np.errstate(all="ignore"):
                    t0 = time.perf_counter_ns()
                    for _ in range(iters):
                        f(*args)
                    out = {"ns": float(time.perf_counter_ns() - t0)}
    except Exception as e:
        out = {"error": "%s: %s" % (type(e).__name__, e)}
    print(json.dumps(out), flush=True)
