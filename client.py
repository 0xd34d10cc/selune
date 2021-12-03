import socket
import asyncio
import json
import functools
import websockets
import click
import itertools

default_url = 'ws://localhost:8088/streams'

STUN_MAGIC = b'\x21\x12\xa4\x42'

def stun_xor(data):
    return bytes(b ^ m for b, m in zip(data, itertools.cycle(STUN_MAGIC)))

def bind_request():
    type = b'\x00\x01' # bind request
    body_len = b'\x00\x00'
    cookie = STUN_MAGIC
    request_id = b'\x01' * 12 # should be random, but whatever
    return type + body_len + cookie + request_id

def parse_stun_response(response):
    attributes = response[20:]

    while attributes:
        type = int.from_bytes(attributes[:2], 'big')
        attr_len = int.from_bytes(attributes[2:4], 'big')
        data = attributes[4:]

        def get_addr(pre=lambda x: x):
            family = data[1]
            # if not IPv4, continue
            if family != 0x01 or attr_len != 8:
                return None
            port = int.from_bytes(pre(data[2:4]), 'big')
            ip = '.'.join(str(octet) for octet in pre(data[4:8]))
            return f'{ip}:{port}'

        if type == 0x01: # MAPPED-ADDRESS
            addr = get_addr()
            if addr:
                return addr
        elif type == 0x20: # XOR-MAPPED-ADDRESS
            addr = get_addr(pre=stun_xor)
            if addr:
                return addr
        else:
            attributes = attributes[4 + attr_len:]

    return None

def public_socket(host, port):
    stun_server = socket.getaddrinfo(
            'stun1.l.google.com',
            0  # port, required
        )[0][4][0]

    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.bind((host, port))
    s.sendto(bind_request(), (stun_server, 19302))
    response, sender = s.recvfrom(1024)
    assert sender[0] == stun_server
    external_addr = parse_stun_response(response)
    return s, external_addr


def coro(f):
    @functools.wraps(f)
    def wrapper(*args, **kwargs):
        return asyncio.run(f(*args, **kwargs))

    return wrapper

async def send(ws, request):
    print('client out:', json.dumps(request, indent=4))
    await ws.send(json.dumps(request))

async def recv(ws):
    response = await ws.recv()
    response = json.loads(response)
    print('client in:', json.dumps(response, indent=4))
    return response

async def request(ws, request):
    await send(ws, request)
    return await recv(ws)

@click.command()
@coro
async def test_remove(url=default_url):
    async with websockets.connect(url) as ws:
        await request(ws, {'type': 'get_streams'})
        response = await request(ws, {
            'type': 'add_stream',
            'stream': {
                'address': 'rtp://127.0.0.1:1337',
                'description': 'A stupid stream'
            }
        })
        stream_id = response['stream_id']

        await request(ws, {'type': 'get_streams'})
        await request(ws, {
            'type': 'remove_stream',
            'stream_id': stream_id
        })
        await request(ws, {'type': 'get_streams'})

@click.command()
@coro
async def viewer(url=default_url):
    s, external_addr = public_socket('0.0.0.0', 3030)
    async with websockets.connect(url) as ws:
        # Wait for a stream
        response = {'streams': {}}
        print('Waiting for stream')
        while True:
            response = await request(ws, {
                'type': 'get_streams'
            })
            if response['streams']:
                break

            await asyncio.sleep(3)
            print('-----------------------')

        # Send watch request
        stream_id, stream_description = list(response['streams'].items())[0]
        streamer_addr = stream_description['address']
        print(f'viewing {stream_id} at {streamer_addr}')
        response = await request(ws, {
            'type': 'watch',
            'stream_id': stream_id,
            'destination': 'rtp://' + external_addr,
        })

        assert response['status'] == 'success'

        # Start sending messages
        s.settimeout(0)
        streamer_ip, streamer_port = streamer_addr[6:].split(':')
        print('streamer at: ', streamer_ip, streamer_port)
        while True:
            s.sendto(b'Hello from viewer', (streamer_ip, int(streamer_port)))

            retry = True
            while retry:
                try:
                    data, sender = s.recvfrom(1024)
                    print(f'{sender} says: {data.decode()}')
                except ConnectionResetError:
                    print(f'{streamer_ip}:{streamer_port} is unreachable')
                except BlockingIOError:
                    retry = False
            await asyncio.sleep(1)



@click.command()
@coro
async def streamer(url=default_url):
    s, external_addr = public_socket('0.0.0.0', 3031)

    async with websockets.connect(url) as ws:
        # Register stream
        await request(ws, {
            'type': 'add_stream',
            'stream': {
                'address': 'rtp://' + external_addr,
                'description': 'A stupid stream'
            }
        })

        # Wait for a viewer
        notification = await recv(ws)
        assert notification['status'] == 'new_viewer'
        viewer_addr = notification['destination']

        # Start sending messages
        s.settimeout(0)
        viewer_ip, viewer_port = viewer_addr[6:].split(':')
        print('viewer at: ', viewer_ip, viewer_port)
        while True:
            s.sendto(b'Hello from streamer', (viewer_ip, int(viewer_port)))

            retry = True
            while retry:
                try:
                    data, sender = s.recvfrom(1024)
                    print(f'{sender} says: {data.decode()}')
                except ConnectionResetError:
                    print(f'{viewer_ip}:{viewer_port} is unreachable')
                except BlockingIOError:
                    retry = False
            await asyncio.sleep(1)

@click.group()
def cli():
    pass

if __name__ == '__main__':
    cli.add_command(test_remove)
    cli.add_command(streamer)
    cli.add_command(viewer)
    cli()