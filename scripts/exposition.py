"""Parse a Prometheus exposition and bind its samples to an expression.

Deliberately not an inline python -c like jqp: the expressions here are long
enough that shell quoting becomes the thing under test, and a quoting mistake
reads as a failing assertion rather than as a quoting mistake.
"""

import re
import sys

SAMPLE = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)\s*$')


def parse(text):
    samples = []
    for line in text.splitlines():
        if not line or line.startswith('#'):
            continue
        m = SAMPLE.match(line)
        if not m:
            continue
        name, raw_labels, value = m.group(1), m.group(2), m.group(3)
        labels = {}
        if raw_labels:
            for pair in re.findall(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"',
                                   raw_labels):
                labels[pair[0]] = pair[1].replace('\\"', '"').replace('\\\\', '\\')
        try:
            number = float(value)
        except ValueError:
            number = float('nan')
        samples.append({'name': name, 'labels': labels, 'value': number})
    return samples


def main():
    samples = parse(sys.stdin.read())
    try:
        print(eval(sys.argv[1], {'samples': samples}))
    except Exception as exc:  # noqa: BLE001 - the point is to report, not raise
        print('<<error: %s>>' % exc)


if __name__ == '__main__':
    main()
