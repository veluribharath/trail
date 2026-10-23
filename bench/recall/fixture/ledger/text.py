def truncate(s, n):
    """Cut s to n characters, ending with an ellipsis when it was longer."""
    if len(s) <= n:
        return s
    return s[: max(0, n - 1)] + "…"
