# rbw1 worker for Julia (refbench). See docs/design/multiref.md, section 3.3.
#
# julia --startup-file=no --history-file=no -t 1 --project=<dir> -e <this file> <kernels.jl>
#
# kernels.jl is trusted, local, committed code. It defines
#
#     KERNELS = Dict{String,Function}("gamma" => x -> SpecialFunctions.gamma(x), ...)
#
# one SCALAR function per name (any arity, Float64 arguments, Float64 result).
# The timed body is `out .= f.(args...)` inside a function barrier on typed
# Vector{Float64}s, with no @fastmath and the default math mode. Nothing read from
# stdin is evaluated: a request names a key of KERNELS, and that is all.
#
# Environment: RBW1_DIR, the run directory; every path of a request must lie
# inside it.
using SHA, TOML, LinearAlgebra

const MAXLINE = 4096
const MAXDATA = 256 << 20
const DIR = get(ENV, "RBW1_DIR", "")

isempty(DIR) && (println("err RBW1_DIR is not set"); exit(1))
isempty(ARGS) && (println("err no kernels file"); exit(1))

include(abspath(ARGS[1]))   # defines KERNELS
@isdefined(KERNELS) || (println("err the kernels file defines no KERNELS"); exit(1))

BLAS.set_num_threads(1)

const DATA = Dict{String,Vector{Vector{Float64}}}()
const ROWS = Dict{String,Int}()
const OUT = Dict{String,Vector{Float64}}()

oneline(s) = replace(first(string(s), 200), r"[^\x20-\x7e]" => "?")

function inside(path::AbstractString)
    (isabspath(path) && normpath(path) == path && !occursin("..", path) &&
     startswith(path, DIR * "/")) || error("path outside the run directory")
    path
end

function load(key, path, cols, rows)
    cols = parse(Int, cols); rows = parse(Int, rows)
    # Each factor is bounded BEFORE the product is formed: Int64 arithmetic wraps.
    (0 < cols <= 1 << 28 && 0 < rows <= 1 << 28 && cols * rows * 8 <= MAXDATA) || error("size out of range")
    inside(path)
    bytes = read(path)
    length(bytes) == cols * rows * 8 || error("file holds $(length(bytes)) bytes, want $(cols * rows * 8)")
    all = reinterpret(Float64, bytes)   # little-endian hosts only; checked below
    ENDIAN_BOM == 0x04030201 || error("big-endian host")
    DATA[key] = [Vector{Float64}(all[(j - 1) * rows + 1:j * rows]) for j in 1:cols]
    ROWS[key] = rows
    OUT[key] = Vector{Float64}(undef, rows)
    "ok " * bytes2hex(sha256(bytes))
end

function kernel(key, fn)
    haskey(DATA, key) || error("key not loaded")
    haskey(KERNELS, fn) || error("function not in the table")
    KERNELS[fn]
end

# The function barrier: f and the argument tuple are concrete here.
function run!(f, out::Vector{Float64}, args::Tuple, iters::Int)
    for _ in 1:iters
        out .= f.(args...)
    end
    return nothing
end

function evalpoints!(f, out::Vector{Float64}, args::Tuple)
    raised = 0
    first = nothing
    for i in eachindex(out)
        out[i] = try
            Float64(f(map(a -> a[i], args)...))
        catch e
            first === nothing && (first = e)
            raised += 1
            NaN
        end
    end
    # A kernel that raises on EVERY point is a broken kernel (wrong arity, a
    # typo), not a domain: refuse instead of reporting an all-NaN column.
    raised == length(out) && error("the kernel raised on every point: " * oneline(sprint(showerror, first)))
    raised
end

function versions()
    v = Pair{String,String}[]
    push!(v, "julia" => string(VERSION))
    push!(v, "llvm" => string(Base.libllvm_version))
    push!(v, "cpu" => Sys.CPU_NAME)
    push!(v, "threads" => string(Threads.nthreads()))
    push!(v, "gc_threads" => string(Threads.ngcthreads()))
    push!(v, "blas_threads" => string(BLAS.get_num_threads()))
    # Every package of the active Manifest.toml: version and git-tree-sha1.
    manifest = joinpath(dirname(Base.active_project()), "Manifest.toml")
    if isfile(manifest)
        deps = get(TOML.parsefile(manifest), "deps", Dict())
        for name in sort!(collect(keys(deps)))
            e = deps[name][1]
            ver = string(get(e, "version", "stdlib"))
            tree = get(e, "git-tree-sha1", "")
            push!(v, name => isempty(tree) ? ver : ver * "@" * tree)
        end
    end
    "v " * join((k * "=" * replace(val, " " => "_") for (k, val) in v), " ")
end

function handle(line::AbstractString)
    length(line) <= MAXLINE || error("line longer than $MAXLINE")
    t = split(line, ' ')
    any(isempty, t) && error("empty token")
    cmd = t[1]
    if cmd == "L" && length(t) == 5
        load(t[2], t[3], t[4], t[5])
    elseif cmd == "W" && length(t) == 3
        f = kernel(t[2], t[3])
        run!(f, OUT[t[2]], Tuple(DATA[t[2]]), 1)
        "ok"
    elseif cmd == "T" && length(t) == 4
        f = kernel(t[2], t[3])
        iters = parse(Int, t[4]); iters > 0 || error("iters must be positive")
        out, args = OUT[t[2]], Tuple(DATA[t[2]])
        t0 = time_ns()
        run!(f, out, args, iters)
        t1 = time_ns()
        "ns " * string(Float64(t1 - t0))
    elseif cmd == "E" && length(t) == 4
        f = kernel(t[2], t[3])
        path = inside(t[4])
        out = Vector{Float64}(undef, ROWS[t[2]])
        raised = evalpoints!(f, out, Tuple(DATA[t[2]]))
        write(path, out)
        "ok $(count(!isfinite, out)) $raised"
    elseif cmd == "V" && length(t) == 1
        versions()
    else
        error("unknown request")
    end
end

for line in eachline(stdin)
    reply = try
        handle(line)
    catch e
        "err " * oneline(e isa ErrorException ? e.msg : sprint(showerror, e))
    end
    println(stdout, reply)
    flush(stdout)
end
