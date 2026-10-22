"""Run from the Calibre source root, using its build-time Python environment."""

import errno
import importlib.util
import multiprocessing
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.getcwd())
import setup  # noqa: F401,E402: initialize Calibre's source and plugin paths
from calibre.utils import lock  # noqa: E402


def load_fixtures(path):
    spec = importlib.util.spec_from_file_location("polish_fixture_under_test", path)
    fixtures = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(fixtures)
    return fixtures


def reader(module_path, cache, helper, fd):
    from multiprocessing.connection import Connection

    fixtures = load_fixtures(module_path)
    fixtures.get_cache = lambda: cache
    original_retry = lock.unix_retry
    with Connection(int(fd)) as conn:
        reported = False

        def retry(err):
            nonlocal reported
            if not reported and err.errno in (errno.EACCES, errno.EAGAIN):
                # Observe an actual failed flock, not just process startup.
                reported = True
                conn.send("blocked")
                if not conn.poll(30):
                    raise AssertionError("writer did not release reader")
                if conn.recv() != "released":
                    raise AssertionError("unexpected writer message")
            return original_retry(err)

        lock.unix_retry = retry
        book = getattr(fixtures, helper)()
        conn.send(Path(book).read_text())


class FixtureLockTest(unittest.TestCase):
    def test_reader_waits_for_writer(self):
        module_path = str(Path(sys.argv[1]).resolve())
        for helper, stem in (("get_simple_book", "simple"), ("get_split_book", "split")):
            with self.subTest(helper=helper), tempfile.TemporaryDirectory() as cache:
                # Never share the parallel suite's fixture cache or user books.
                book = Path(cache, stem + ".epub")
                source = Path(module_path).with_name(stem + ".html")
                fresh = source.stat().st_mtime + 60
                parent, child = multiprocessing.Pipe()
                with parent, child:
                    with lock.ExclusiveFile(str(book) + ".lock"):
                        book.write_text("partial")
                        os.utime(book, (fresh, fresh))
                        env = os.environ.copy()
                        # Exercise the mtime fast path even when CI forces a build.
                        env["CI"] = "false"
                        proc = subprocess.Popen(
                            [sys.executable, __file__, module_path, "--reader", cache, helper, str(child.fileno())],
                            pass_fds=(child.fileno(),), env=env,
                        )
                        child.close()
                        try:
                            self.assertTrue(parent.poll(30), "reader did not report lock contention")
                            self.assertEqual(parent.recv(), "blocked", "reader returned an unfinished fixture")
                            book.write_text("complete")
                            os.utime(book, (fresh, fresh))
                        except BaseException:
                            proc.kill()
                            proc.wait(timeout=10)
                            raise
                    try:
                        parent.send("released")
                        self.assertTrue(parent.poll(30), "reader did not finish after writer released its lock")
                        self.assertEqual(parent.recv(), "complete")
                        self.assertEqual(proc.wait(timeout=10), 0)
                    finally:
                        if proc.poll() is None:
                            proc.kill()
                            proc.wait(timeout=10)


if __name__ == "__main__":
    if len(sys.argv) > 2 and sys.argv[2] == "--reader":
        reader(sys.argv[1], *sys.argv[3:])
    else:
        unittest.main(argv=[sys.argv[0]], verbosity=2)
