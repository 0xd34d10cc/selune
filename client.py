import asyncio
import json
import websockets


async def send_request(ws, request):
    await ws.send(json.dumps(request).encode('utf-8'))
    response = await ws.recv()
    return json.loads(response.decode('utf-8'))

async def main(uri):
    async with websockets.connect(uri) as ws:
        response = await send_request(ws, {'type': 'get_streams'})
        print('streams: ', response)

        response = await send_request(ws, {
            'type': 'add_stream',
            'stream': {
                'address': 'rtp://127.0.0.1:1337',
                'description': 'A stupid stream'
            }
        })

        print('add_stream status: ', response)
        stream_id = response['stream_id']

        response = await send_request(ws, {'type': 'get_streams'})
        print('streams: ', response)

        response = await send_request(ws, {
            'type': 'remove_stream',
            'stream_id': stream_id
        })
        print('remove_steram status: ', response)

        response = await send_request(ws, {'type': 'get_streams'})
        print('streams: ', response)

if __name__ == '__main__':
    asyncio.get_event_loop().run_until_complete(main('ws://localhost:8080/streams'))