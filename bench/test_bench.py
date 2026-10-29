#!/usr/bin/env python3
"""Tests of the benchmark's helpers. Run with: python3 -I test_bench.py -v

They need nothing but Python: no Terraform, Vault or Docker.
"""
import json, os, re, shutil, socket, subprocess, sys, tempfile, time, unittest
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))


def run(*args, env=None):
    full = dict(os.environ)
    full.update(env or {})
    return subprocess.run([sys.executable, "-I"] + list(args), check=True,
                          capture_output=True, text=True, env=full).stdout


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Mock:
    """The Google API mock, started on a port of its own."""

    def __init__(self, *args):
        self.port = free_port()
        self.proc = subprocess.Popen([sys.executable, "-I", os.path.join(HERE, "gcpmock.py"), str(self.port)] + list(args))
        for _ in range(100):
            try:
                socket.create_connection(("127.0.0.1", self.port), timeout=0.1).close()
                return
            except OSError:
                time.sleep(0.05)
        raise RuntimeError("the mock didn't start")

    def request(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request("http://127.0.0.1:%d%s" % (self.port, path), data=data, method=method)
        try:
            with urllib.request.urlopen(req) as resp:
                return resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def stop(self):
        self.proc.terminate()
        self.proc.wait()


def generate(out, modules, per_module, **env):
    env = {"COPIES": "1", **{k: str(v) for k, v in env.items()}}
    run(os.path.join(HERE, "gen4.py"), out, str(modules), str(per_module), env=env)


def read(path):
    with open(path) as f:
        return f.read()


def read_tree(root):
    files = {}
    for dirpath, _, names in os.walk(root):
        for name in names:
            path = os.path.join(dirpath, name)
            files[os.path.relpath(path, root)] = read(path)
    return files


class TestBench(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()

    def tearDown(self):
        shutil.rmtree(self.tmp)

    def test_mock_count_dump_load(self):
        mock = Mock()
        try:
            for name in ("a", "b"):
                status, _ = mock.request("PUT", "/v1/projects/tfperf/topics/" + name, {"labels": {"x": name}})
                self.assertEqual(status, 200)
            mock.request("POST", "/v1/projects/tfperf/secrets?secretId=s", {})
            self.assertEqual(mock.request("GET", "/_count?prefix=projects/tfperf/topics/"), (200, {"count": 2}))
            status, dump = mock.request("GET", "/_dump")
            self.assertEqual(status, 200)
            _, topic = mock.request("GET", "/v1/projects/tfperf/topics/a")
        finally:
            mock.stop()
        saved = os.path.join(self.tmp, "mock.json")
        with open(saved, "w") as f:
            json.dump(dump, f)

        mock = Mock("--load", saved)
        try:
            self.assertEqual(mock.request("GET", "/v1/projects/tfperf/topics/a"), (200, topic))
            self.assertEqual(mock.request("GET", "/_count?prefix=projects/tfperf/"), (200, {"count": 3}))
        finally:
            mock.stop()

    def test_blob(self):
        out = os.path.join(self.tmp, "cfg")
        generate(out, 3, 4, BLOB_KB=2)
        root = read(os.path.join(out, "main.tf"))
        literals = re.findall(r'blob\s*=\s*"([^"]*)"', root)
        self.assertEqual(len(literals), 1)
        self.assertEqual(len(literals[0]), 2048)
        self.assertNotIn("${", literals[0])
        self.assertEqual(len(re.findall(r"blob\s*=\s*local\.blob", root)), 3)
        for i in range(3):
            module = read(os.path.join(out, "modules", "app_%03d" % i, "main.tf"))
            self.assertRegex(module, r'variable "blob"')
            data_json = module[module.index("data_json"):module.index("custom_metadata")]
            self.assertRegex(data_json, r"blob\s*=\s*var\.blob")

        # Without BLOB_KB the configuration is what it always was.
        plain = os.path.join(self.tmp, "plain")
        generate(plain, 3, 4)
        self.assertNotIn("blob", "".join(read_tree(plain).values()))

    def mkchanges(self, out, target, mix, seed=1):
        return json.loads(run(os.path.join(HERE, "mkchanges.py"), out, str(target), mix, str(seed)))

    def test_mkchanges_mixed(self):
        out = os.path.join(self.tmp, "cfg")
        generate(out, 20, 10)
        pristine = os.path.join(self.tmp, "pristine")
        shutil.copytree(out, pristine)
        counts = self.mkchanges(out, 100, "mixed")
        for kind, want in (("update", 70), ("create", 10), ("delete", 10), ("replace", 10)):
            self.assertLessEqual(abs(counts[kind] - want), want * 0.25, (kind, counts))
        # An updated module's topics all get the new label: one line per
        # module, ten topics each.
        bench2 = sum(text.count('"bench2"') for text in read_tree(out).values())
        self.assertEqual(bench2 * 10, counts["update"])

        again = os.path.join(self.tmp, "again")
        shutil.copytree(pristine, again)
        self.assertEqual(self.mkchanges(again, 100, "mixed"), counts)
        self.assertEqual(read_tree(again), read_tree(out))

    def test_mkchanges_create(self):
        out = os.path.join(self.tmp, "cfg")
        generate(out, 20, 10)
        counts = self.mkchanges(out, 50, "create")
        self.assertEqual((counts["update"], counts["delete"], counts["replace"]), (0, 0, 0))
        self.assertLessEqual(abs(counts["create"] - 50), 50 * 0.25, counts)


if __name__ == "__main__":
    unittest.main()
