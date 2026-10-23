# New task: nothing in the history is about it.
import unittest

from ledger.text import slugify


class Slugify(unittest.TestCase):
    def test_slugify(self):
        self.assertEqual(slugify("Hello, World!"), "hello-world")
        self.assertEqual(slugify("  Invoice #42 -- March  "), "invoice-42-march")
        self.assertEqual(slugify("---"), "")
        self.assertEqual(slugify("a_b c"), "a-b-c")
