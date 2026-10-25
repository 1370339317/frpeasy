import re, io, os

BS = chr(92)

def norm(p):
    return os.path.normpath(p).replace(BS, '/')

def slug(t):
    t = t.strip().lower()
    t = re.sub(r'[`*_]', '', t)
    t = re.sub(r'[^\w\s\u4e00-\u9fff-]', '', t)
    return re.sub(r'\s+', '-', t).strip('-')

files = ['README.md', 'QUICKSTART.md', 'docs/CONVENTIONS.md']
anchors = {}
for f in files:
    anchors[norm(f)] = set()
    for line in io.open(f, encoding='utf-8'):
        if line.startswith('#'):
            anchors[norm(f)].add(slug(line.lstrip('#').strip()))

bad = 0
for f in files:
    for m in re.finditer(r'\[([^\]]+)\]\(([^)]+)\)', io.open(f, encoding='utf-8').read()):
        target = m.group(2)
        if target.startswith('http'):
            continue
        path, _, frag = target.partition('#')
        path = norm(os.path.join(os.path.dirname(f), path or f))
        if not os.path.exists(path):
            print('MISSING FILE', f, '->', target); bad += 1
        elif frag and frag not in anchors.get(path, set()):
            print('BAD ANCHOR  ', f, '->', target); bad += 1
print('checked %d files, problems = %d' % (len(files), bad))
